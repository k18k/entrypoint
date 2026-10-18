package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tok := "ept_0123456789ab_" + strings.Repeat("A", 43)
	s := String("token=" + tok + " pakiet=epc1." + strings.Repeat("x", 60) + " klucz=-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----")
	if strings.Contains(s, tok) || strings.Contains(s, "epc1.xxx") || strings.Contains(s, "abc") {
		t.Fatalf("niezamaskowane: %s", s)
	}
}
