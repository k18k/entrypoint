// Pakiet token: format tokenów serwisów i pakietu połączenia ENTRYPOINT_CONNECT.
//
// Token ma postać ept_<id>_<sekret>. Identyfikator (12 znaków hex) jest jawny
// i służy do wyszukania wpisu; sekret to 256 losowych bitów w base64url.
// W bazie zapisywany jest wyłącznie SHA-256 sekretu.
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

// ErrMalformed oznacza token o niepoprawnej budowie. Komunikat nie zawiera treści tokenu.
var ErrMalformed = errors.New("niepoprawny format tokenu")

// New tworzy nowy token. Zwraca pełną wartość (pokazywaną raz), identyfikator i hash sekretu.
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

// Parse rozdziela token na identyfikator i sekret.
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

// Hash zwraca hex SHA-256 sekretu. Sekret ma 256 bitów entropii, więc wolny hash nie jest potrzebny.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Verify porównuje sekret z hashem w stałym czasie.
func Verify(secret, hash string) bool {
	got := Hash(secret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// Bundle to zawartość ENTRYPOINT_CONNECT.
type Bundle struct {
	V       int      `json:"v"`
	Server  string   `json:"server"` // nazwa DNS bramy, nigdy adres IP
	Port    int      `json:"port"`
	CAs     []string `json:"ca"` // zaufane CA w PEM
	Service string   `json:"service"`
	Token   string   `json:"token"`
}

const bundlePrefix = "epc1."

// Encode koduje pakiet połączenia.
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

// DecodeBundle dekoduje i sprawdza pakiet połączenia. Błędy nie zawierają jego treści.
func DecodeBundle(s string) (Bundle, error) {
	var b Bundle
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, bundlePrefix) {
		return b, errors.New("ENTRYPOINT_CONNECT: nieznany format pakietu połączenia")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(bundlePrefix):])
	if err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: uszkodzone kodowanie pakietu")
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: uszkodzona treść pakietu")
	}
	switch {
	case b.V != 1:
		return b, fmt.Errorf("ENTRYPOINT_CONNECT: nieobsługiwana wersja pakietu %d", b.V)
	case b.Server == "" || b.Port <= 0 || b.Port > 65535:
		return b, errors.New("ENTRYPOINT_CONNECT: brak nazwy lub portu bramy")
	case len(b.CAs) == 0:
		return b, errors.New("ENTRYPOINT_CONNECT: brak zaufanego CA")
	}
	if _, _, err := Parse(b.Token); err != nil {
		return b, errors.New("ENTRYPOINT_CONNECT: niepoprawny token")
	}
	return b, nil
}
