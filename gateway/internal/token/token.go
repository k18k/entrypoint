// Package token: the format of service tokens and of the ENTRYPOINT_CONNECT connection bundle.
//
// A token looks like ept_<id>_<secret>. The ID (12 hex characters) is public
// and used to look up the entry; the secret is 256 random bits in base64url.
// Only the SHA-256 of the secret is stored in the database.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const prefix = "ept_"

var (
	idRe     = regexp.MustCompile(`^[0-9a-f]{12}$`)
	secretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
)

// ErrMalformed means a malformed token. The message never contains the token.
var ErrMalformed = errors.New("malformed token")

// New creates a token. It returns the full value (shown once), the ID and the secret's hash.
func New() (full, id, hash string, err error) {
	idb := make([]byte, 6)
	sec := make([]byte, 32)
	if _, err = rand.Read(idb); err != nil {
		return
	}
	if _, err = rand.Read(sec); err != nil {
		return
	}
	id = hex.EncodeToString(idb)
	s := base64.RawURLEncoding.EncodeToString(sec)
	return prefix + id + "_" + s, id, Hash(s), nil
}

// Parse splits a token into its ID and secret.
func Parse(full string) (id, secret string, err error) {
	if !strings.HasPrefix(full, prefix) {
		return "", "", ErrMalformed
	}
	rest := full[len(prefix):]
	i := strings.IndexByte(rest, '_')
	if i < 0 {
		return "", "", ErrMalformed
	}
	id, secret = rest[:i], rest[i+1:]
	if !idRe.MatchString(id) || !secretRe.MatchString(secret) {
		return "", "", ErrMalformed
	}
	return id, secret, nil
}

// Hash returns the hex SHA-256 of the secret. The secret has 256 bits of entropy, so a slow hash is not needed.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Verify compares the secret with the hash in constant time.
func Verify(secret, hash string) bool {
	got := Hash(secret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// Bundle is the content of ENTRYPOINT_CONNECT.
type Bundle struct {
	V       int      `json:"v"`
	Server  string   `json:"server"` // gateway DNS name, never an IP address
	Port    int      `json:"port"`
	CAs     []string `json:"ca"` // trusted CAs in PEM
	Service string   `json:"service"`
	Token   string   `json:"token"`
}

const bundlePrefix = "epc1."

// Encode encodes the connection bundle.
func (b Bundle) Encode() (string, error) {
	if b.V == 0 {
		b.V = 1
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return bundlePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeBundle decodes and checks a connection bundle. Errors never contain its content.
func DecodeBundle(s string) (Bundle, error) {
	var b Bundle
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, bundlePrefix) {
		return b, errors.New("ENTRYPOINT_CONNECT: unknown connection bundle format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(bundlePrefix):])
	if err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: corrupted bundle encoding")
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: corrupted bundle content")
	}
	switch {
	case b.V != 1:
		return b, fmt.Errorf("ENTRYPOINT_CONNECT: unsupported bundle version %d", b.V)
	case b.Server == "" || b.Port <= 0 || b.Port > 65535:
		return b, errors.New("ENTRYPOINT_CONNECT: missing gateway name or port")
	case len(b.CAs) == 0:
		return b, errors.New("ENTRYPOINT_CONNECT: no trusted CA")
	}
	if _, _, err := Parse(b.Token); err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: invalid token")
	}
	return b, nil
}
