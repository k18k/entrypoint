package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tok := "ept_0123456789ab_" + strings.Repeat("A", 43)
	s := String("token=" + tok + " bundle=epc1." + strings.Repeat("x", 60) + " key=-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----")
	s += String(" vpn=epv1." + strings.Repeat("y", 60) + " hs=hskey-auth-" + strings.Repeat("z", 40))
	if strings.Contains(s, tok) || strings.Contains(s, "epc1.xxx") || strings.Contains(s, "abc") || strings.Contains(s, "yyy") || strings.Contains(s, "zzz") {
		t.Fatalf("not masked: %s", s)
	}
}
