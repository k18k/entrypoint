// Pakiet store przechowuje stan bramy w SQLite: rewizje konfiguracji,
// trwałe rezerwacje portów, dziennik odwołań (tylko do dopisywania) i logowania tuneli.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/k18k/entrypoint/gateway/internal/spec"
)

// Statusy rewizji.
const (
	StatusPending  = "pending"
	StatusApplying = "applying"
	StatusApplied  = "applied"
	StatusFailed   = "failed"
)

var ErrConflict = errors.New("konflikt rewizji bazowej")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS revisions(
  num INTEGER PRIMARY KEY,
  base INTEGER NOT NULL,
  author TEXT NOT NULL,
  created_at TEXT NOT NULL,
  summary TEXT NOT NULL,
  diff TEXT NOT NULL,
  spec TEXT NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS components(
  name TEXT PRIMARY KEY,
  applied INTEGER NOT NULL,
  hash TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ports(
  port INTEGER PRIMARY KEY,
  service_id INTEGER NOT NULL UNIQUE,
  service_name TEXT NOT NULL,
  reserved_at TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS ports_no_delete BEFORE DELETE ON ports
BEGIN SELECT RAISE(ABORT, 'rezerwacje portów są trwałe'); END;
CREATE TRIGGER IF NOT EXISTS ports_no_update BEFORE UPDATE ON ports
BEGIN SELECT RAISE(ABORT, 'rezerwacje portów są trwałe'); END;
CREATE TABLE IF NOT EXISTS revocations(
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  token_id TEXT NOT NULL,
  revoked_at TEXT NOT NULL,
  reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS revocations_token ON revocations(token_id);
CREATE TRIGGER IF NOT EXISTS revocations_no_update BEFORE UPDATE ON revocations
BEGIN SELECT RAISE(ABORT, 'dziennik odwołań jest tylko do dopisywania'); END;
CREATE TRIGGER IF NOT EXISTS revocations_no_delete BEFORE DELETE ON revocations
BEGIN SELECT RAISE(ABORT, 'dziennik odwołań jest tylko do dopisywania'); END;
CREATE TABLE IF NOT EXISTS logins(
  token_id TEXT PRIMARY KEY,
  last_login TEXT NOT NULL,
  client_ip TEXT NOT NULL,
  hostname TEXT NOT NULL,
  version TEXT NOT NULL,
  ca_fps TEXT NOT NULL
);
`

// Open otwiera bazę i tworzy schemat.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schemat bazy: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Revision to wpis historii konfiguracji.
type Revision struct {
	Num       int64      `json:"num"`
	Base      int64      `json:"base"`
	Author    string     `json:"author"`
	CreatedAt time.Time  `json:"createdAt"`
	Summary   string     `json:"summary"`
	Diff      []string   `json:"diff"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	Attempts  int        `json:"attempts"`
	Spec      *spec.Spec `json:"-"`
}

func scanRevision(row interface{ Scan(...any) error }) (*Revision, error) {
	var r Revision
	var created, diff, specJSON string
	if err := row.Scan(&r.Num, &r.Base, &r.Author, &created, &r.Summary, &diff, &specJSON, &r.Status, &r.Error, &r.Attempts); err != nil {
		return nil, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	_ = json.Unmarshal([]byte(diff), &r.Diff)
	r.Spec = &spec.Spec{}
	if err := json.Unmarshal([]byte(specJSON), r.Spec); err != nil {
		return nil, fmt.Errorf("rewizja %d: uszkodzona konfiguracja: %w", r.Num, err)
	}
	r.Spec.Normalize()
	return &r, nil
}

const revCols = `num, base, author, created_at, summary, diff, spec, status, error, attempts`

// Head zwraca najnowszą rewizję, która nie zakończyła się błędem (bieżący stan oczekiwany).
func (s *Store) Head(ctx context.Context) (*Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+revCols+` FROM revisions WHERE status != 'failed' ORDER BY num DESC LIMIT 1`)
	r, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// LastApplied zwraca ostatnią w pełni zastosowaną rewizję.
func (s *Store) LastApplied(ctx context.Context) (*Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+revCols+` FROM revisions WHERE status = 'applied' ORDER BY num DESC LIMIT 1`)
	r, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) Revision(ctx context.Context, num int64) (*Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+revCols+` FROM revisions WHERE num = ?`, num)
	r, err := scanRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) Revisions(ctx context.Context, limit int) ([]*Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+revCols+` FROM revisions ORDER BY num DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Mutation zmienia konfigurację w transakcji tworzenia rewizji.
// Dostaje kopię bieżącej konfiguracji i transakcję (do rezerwacji portów).
type Mutation func(tx *Tx, s *spec.Spec) (summary string, err error)

type Tx struct {
	tx  *sql.Tx
	ctx context.Context
}

// ReservePort trwale rezerwuje pierwszy wolny port z puli dla nowego serwisu.
// Zwraca identyfikator serwisu i port. Rezerwacja nigdy nie jest zwalniana.
func (t *Tx) ReservePort(name string, start, end int) (int64, int, error) {
	rows, err := t.tx.QueryContext(t.ctx, `SELECT port FROM ports WHERE port BETWEEN ? AND ? ORDER BY port`, start, end)
	if err != nil {
		return 0, 0, err
	}
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, 0, err
		}
		used[p] = true
	}
	rows.Close()
	port := 0
	for p := start; p <= end; p++ {
		if !used[p] {
			port = p
			break
		}
	}
	if port == 0 {
		return 0, 0, errors.New("pula portów serwisów jest wyczerpana")
	}
	var id int64
	if err := t.tx.QueryRowContext(t.ctx, `SELECT COALESCE(MAX(service_id), 0) + 1 FROM ports`).Scan(&id); err != nil {
		return 0, 0, err
	}
	if _, err := t.tx.ExecContext(t.ctx, `INSERT INTO ports(port, service_id, service_name, reserved_at) VALUES(?, ?, ?, ?)`, port, id, name, now()); err != nil {
		return 0, 0, err
	}
	return id, port, nil
}

// Revoke dopisuje odwołanie w tej samej transakcji.
func (t *Tx) Revoke(tokenID, reason string) error {
	_, err := t.tx.ExecContext(t.ctx, `INSERT INTO revocations(token_id, revoked_at, reason) VALUES(?, ?, ?)`, tokenID, now(), reason)
	return err
}

// Validator sprawdza konfigurację przed zapisem (schemat, konflikty, render Caddy).
type Validator func(*spec.Spec) error

// Create tworzy nową rewizję na podstawie bieżącej. baseRev < 0 oznacza „bieżąca głowa”.
func (s *Store) Create(ctx context.Context, baseRev int64, author string, mut Mutation, validate Validator) (*Revision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var headNum int64
	var headSpec = &spec.Spec{}
	row := tx.QueryRowContext(ctx, `SELECT num, spec FROM revisions WHERE status != 'failed' ORDER BY num DESC LIMIT 1`)
	var specJSON string
	switch err := row.Scan(&headNum, &specJSON); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal([]byte(specJSON), headSpec); err != nil {
			return nil, err
		}
	}
	headSpec.Normalize()
	if baseRev >= 0 && baseRev != headNum {
		return nil, fmt.Errorf("%w: zmiana oparta na rewizji %d, bieżąca to %d", ErrConflict, baseRev, headNum)
	}
	next := headSpec.Clone()
	summary, err := mut(&Tx{tx: tx, ctx: ctx}, next)
	if err != nil {
		return nil, err
	}
	next.Normalize()
	if validate != nil {
		if err := validate(next); err != nil {
			return nil, err
		}
	}
	diff := spec.Diff(headSpec, next)
	diffJSON, _ := json.Marshal(diff)
	nextJSON, _ := json.Marshal(next)
	var maxNum int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(num), 0) FROM revisions`).Scan(&maxNum); err != nil {
		return nil, err
	}
	num := maxNum + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO revisions(num, base, author, created_at, summary, diff, spec, status) VALUES(?, ?, ?, ?, ?, ?, ?, 'pending')`,
		num, headNum, author, now(), summary, string(diffJSON), string(nextJSON)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Revision(ctx, num)
}

// SetStatus zmienia status rewizji.
func (s *Store) SetStatus(ctx context.Context, num int64, status, errText string) error {
	q := `UPDATE revisions SET status = ?, error = ? WHERE num = ?`
	if status == StatusApplying {
		q = `UPDATE revisions SET status = ?, error = ?, attempts = attempts + 1 WHERE num = ?`
	}
	_, err := s.db.ExecContext(ctx, q, status, errText, num)
	return err
}

// Component to stan zastosowania konfiguracji w komponencie.
type Component struct {
	Name      string    `json:"name"`
	Applied   int64     `json:"applied"`
	Hash      string    `json:"hash"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Store) SetComponent(ctx context.Context, name string, rev int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO components(name, applied, hash, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET applied = excluded.applied, hash = excluded.hash, updated_at = excluded.updated_at`, name, rev, hash, now())
	return err
}

func (s *Store) Components(ctx context.Context) ([]Component, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, applied, hash, updated_at FROM components ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Component
	for rows.Next() {
		var c Component
		var t string
		if err := rows.Scan(&c.Name, &c.Applied, &c.Hash, &t); err != nil {
			return nil, err
		}
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Revocation to wpis dziennika odwołań.
type Revocation struct {
	Seq       int64     `json:"seq"`
	TokenID   string    `json:"tokenId"`
	RevokedAt time.Time `json:"revokedAt"`
	Reason    string    `json:"reason"`
}

// Revoke dopisuje odwołanie tokenu.
func (s *Store) Revoke(ctx context.Context, tokenID, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO revocations(token_id, revoked_at, reason) VALUES(?, ?, ?)`, tokenID, now(), reason)
	return err
}

// RevokedSet zwraca zbiór odwołanych identyfikatorów tokenów.
func (s *Store) RevokedSet(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT token_id FROM revocations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) Revocations(ctx context.Context) ([]Revocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, token_id, revoked_at, reason FROM revocations ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revocation
	for rows.Next() {
		var r Revocation
		var t string
		if err := rows.Scan(&r.Seq, &r.TokenID, &t, &r.Reason); err != nil {
			return nil, err
		}
		r.RevokedAt, _ = time.Parse(time.RFC3339Nano, t)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReservedPorts zwraca wszystkie rezerwacje portów (także usuniętych serwisów).
func (s *Store) ReservedPorts(ctx context.Context) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port, service_name FROM ports`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var p int
		var n string
		if err := rows.Scan(&p, &n); err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, rows.Err()
}

// Login to ostatnie logowanie tunelu danym tokenem.
type Login struct {
	TokenID   string    `json:"tokenId"`
	LastLogin time.Time `json:"lastLogin"`
	ClientIP  string    `json:"clientIp"`
	Hostname  string    `json:"hostname"`
	Version   string    `json:"version"`
	CAFps     []string  `json:"caFingerprints"`
}

func (s *Store) RecordLogin(ctx context.Context, l Login) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO logins(token_id, last_login, client_ip, hostname, version, ca_fps) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(token_id) DO UPDATE SET last_login = excluded.last_login, client_ip = excluded.client_ip,
		hostname = excluded.hostname, version = excluded.version, ca_fps = excluded.ca_fps`,
		l.TokenID, l.LastLogin.UTC().Format(time.RFC3339Nano), l.ClientIP, l.Hostname, l.Version, strings.Join(l.CAFps, ","))
	return err
}

func (s *Store) Logins(ctx context.Context) (map[string]Login, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token_id, last_login, client_ip, hostname, version, ca_fps FROM logins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Login{}
	for rows.Next() {
		var l Login
		var t, fps string
		if err := rows.Scan(&l.TokenID, &t, &l.ClientIP, &l.Hostname, &l.Version, &fps); err != nil {
			return nil, err
		}
		l.LastLogin, _ = time.Parse(time.RFC3339Nano, t)
		if fps != "" {
			l.CAFps = strings.Split(fps, ",")
		}
		out[l.TokenID] = l
	}
	return out, rows.Err()
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Snapshot tworzy spójną kopię bazy bez zatrzymywania usług.
func (s *Store) Snapshot(ctx context.Context, dest string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dest)
	return err
}

// RevokeAll odwołuje wszystkie tokeny obecne w dowolnej rewizji (awaryjny restore).
func (s *Store) RevokeAll(ctx context.Context, reason string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM revisions`)
	if err != nil {
		return 0, err
	}
	ids := map[string]bool{}
	for rows.Next() {
		var sj string
		if err := rows.Scan(&sj); err != nil {
			rows.Close()
			return 0, err
		}
		var sp spec.Spec
		if json.Unmarshal([]byte(sj), &sp) == nil {
			for _, t := range sp.Tokens {
				ids[t.ID] = true
			}
		}
	}
	rows.Close()
	revoked, err := s.RevokedSet(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for id := range ids {
		if revoked[id] {
			continue
		}
		if err := s.Revoke(ctx, id, reason); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
