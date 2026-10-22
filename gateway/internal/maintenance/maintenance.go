// Package maintenance is the gateway's maintenance mode during an update: administrative changes
// are refused, except token revocations, which are also journaled so a rollback cannot lose them.
package maintenance

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// State is an active maintenance window.
type State struct {
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
}

// Revocation is a token revoked during maintenance.
type Revocation struct {
	TokenID string    `json:"tokenId"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

func statePath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "state", "maintenance.json")
}
func journalPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "state", "maintenance-revocations.jsonl")
}

// Get returns the active maintenance window, or nil.
func Get(cfg config.Config) *State {
	b, err := os.ReadFile(statePath(cfg))
	if err != nil {
		return nil
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return &State{Reason: "maintenance"} // unreadable: stay on the safe side
	}
	return &s
}

// Enable starts a maintenance window with an empty revocation journal.
func Enable(cfg config.Config, reason string) error {
	b, _ := json.Marshal(State{Since: time.Now().UTC(), Reason: reason})
	_ = os.Remove(journalPath(cfg))
	tmp := statePath(cfg) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(cfg))
}

// Disable ends the window and drops its journal.
func Disable(cfg config.Config) error {
	if err := os.Remove(statePath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(journalPath(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Record journals a revocation made during maintenance (fsynced before the revocation is confirmed).
func Record(cfg config.Config, r Revocation) error {
	f, err := os.OpenFile(journalPath(cfg), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Revocations returns the revocations journaled during the window.
func Revocations(cfg config.Config) ([]Revocation, error) {
	f, err := os.Open(journalPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Revocation
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Revocation
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.TokenID != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
