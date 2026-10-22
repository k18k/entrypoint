package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// RegistrationTTL is how long a link from `entrypoint panel register` stays valid.
const RegistrationTTL = time.Minute

const (
	keysFile         = "passkeys.json"
	registrationFile = "registration.json"
)

// Key is one registered passkey of the administrator.
type Key struct {
	ID         string              `json:"id"` // short, for the CLI
	Name       string              `json:"name"`
	CreatedAt  time.Time           `json:"createdAt"`
	LastUsed   *time.Time          `json:"lastUsed,omitempty"`
	Credential webauthn.Credential `json:"credential"`
}

// Keys is the administrator account: a random user handle and the passkeys. Generation changes
// whenever a key is added or removed, which ends every session.
type Keys struct {
	Generation int    `json:"generation"`
	UserID     []byte `json:"userId"`
	Keys       []Key  `json:"keys"`
}

type registration struct {
	Hash    string    `json:"hash"` // SHA-256 of the token; the token itself is only in the link
	Expires time.Time `json:"expires"`
}

func keysPath(cfg config.Config) string { return filepath.Join(cfg.PanelDir(), keysFile) }
func regPath(cfg config.Config) string  { return filepath.Join(cfg.PanelDir(), registrationFile) }

// LoadKeys reads the passkeys; a missing file means none are registered yet.
func LoadKeys(cfg config.Config) (*Keys, error) {
	b, err := os.ReadFile(keysPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return &Keys{}, nil
	}
	if err != nil {
		return nil, err
	}
	var k Keys
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, fmt.Errorf("%s: %w", keysPath(cfg), err)
	}
	return &k, nil
}

// SaveKeys writes the passkeys atomically, owned by the panel's user (the CLI runs as root).
func SaveKeys(cfg config.Config, k *Keys) error {
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return writePanelFile(keysPath(cfg), b)
}

func writePanelFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if u, err := user.Lookup(config.UserPanel); err == nil {
			uid, _ := strconv.Atoi(u.Uid)
			gid, _ := strconv.Atoi(u.Gid)
			if err := os.Chown(tmp.Name(), uid, gid); err != nil {
				return err
			}
		}
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RemoveKey deletes a passkey by its short ID and ends every session.
func RemoveKey(cfg config.Config, id string) error {
	k, err := LoadKeys(cfg)
	if err != nil {
		return err
	}
	out := k.Keys[:0]
	for _, x := range k.Keys {
		if x.ID != id {
			out = append(out, x)
		}
	}
	if len(out) == len(k.Keys) {
		return fmt.Errorf("no passkey %q; see: entrypoint panel keys", id)
	}
	k.Keys = out
	k.Generation++
	return SaveKeys(cfg, k)
}

// NewRegistration creates a one-time registration token valid for RegistrationTTL; a newer one
// replaces it. Only the token's hash is stored.
func NewRegistration(cfg config.Config) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(tok))
	r := registration{Hash: hex.EncodeToString(sum[:]), Expires: time.Now().Add(RegistrationTTL).UTC()}
	raw, _ := json.Marshal(r)
	if err := writePanelFile(regPath(cfg), raw); err != nil {
		return "", time.Time{}, err
	}
	return tok, r.Expires, nil
}

// consumeRegistration checks the token and deletes it, so a link works once.
func consumeRegistration(cfg config.Config, tok string, now time.Time) error {
	b, err := os.ReadFile(regPath(cfg))
	if err != nil {
		return errors.New("no registration link is active; run: sudo entrypoint panel register")
	}
	var r registration
	if json.Unmarshal(b, &r) != nil {
		return errors.New("invalid registration state")
	}
	sum := sha256.Sum256([]byte(tok))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(r.Hash)) != 1 {
		return errors.New("this registration link is not valid; run: sudo entrypoint panel register")
	}
	if now.After(r.Expires) {
		_ = os.Remove(regPath(cfg))
		return errors.New("this registration link has expired; run: sudo entrypoint panel register")
	}
	return os.Remove(regPath(cfg))
}

// admin is the single panel account as a WebAuthn user.
type admin struct {
	id    []byte
	creds []webauthn.Credential
}

func (a *admin) WebAuthnID() []byte                         { return a.id }
func (a *admin) WebAuthnName() string                       { return "admin" }
func (a *admin) WebAuthnDisplayName() string                { return "Entrypoint administrator" }
func (a *admin) WebAuthnCredentials() []webauthn.Credential { return a.creds }

func (k *Keys) admin() *admin {
	a := &admin{id: k.UserID}
	for _, x := range k.Keys {
		a.creds = append(a.creds, x.Credential)
	}
	return a
}

func shortID(credID []byte) string {
	sum := sha256.Sum256(credID)
	return hex.EncodeToString(sum[:6])
}
