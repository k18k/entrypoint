package token

import (
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	full, id, hash, err := New()
	if err != nil {
		t.Fatal(err)
	}
	gid, sec, err := Parse(full)
	if err != nil || gid != id || !Verify(sec, hash) {
		t.Fatalf("parse/verify: %v", err)
	}
	if Verify(sec+"x", hash) {
		t.Fatal("zły sekret przechodzi")
	}
	for _, bad := range []string{"", "ept_", "ept_123_abc", "xpt_" + full[4:], full + "!"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("przyjęto %q", bad)
		}
	}
}

func TestBundle(t *testing.T) {
	full, _, _, _ := New()
	b := Bundle{Server: "tunnel.example.com", Port: 55555, CAs: []string{"-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"}, Service: "api", Token: full}
	enc, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBundle(enc)
	if err != nil || got.Token != full || got.Server != b.Server {
		t.Fatalf("decode: %v", err)
	}
	// Błędy nie zawierają treści pakietu ani tokenu.
	for _, bad := range []string{"epc1.@@", enc[:len(enc)-5], "xyz"} {
		_, err := DecodeBundle(bad)
		if err == nil {
			t.Errorf("przyjęto %q", bad)
			continue
		}
		if strings.Contains(err.Error(), full) || strings.Contains(err.Error(), "epc1.") && len(err.Error()) > 80 {
			t.Errorf("błąd ujawnia treść: %v", err)
		}
	}
	noCA := b
	noCA.CAs = nil
	e2, _ := noCA.Encode()
	if _, err := DecodeBundle(e2); err == nil {
		t.Error("pakiet bez CA przyjęty")
	}
}
