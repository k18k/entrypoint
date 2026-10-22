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
		t.Fatal("wrong secret passes")
	}
	for _, bad := range []string{"", "ept_", "ept_123_abc", "xpt_" + full[4:], full + "!"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
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
	// Errors contain neither the bundle nor the token.
	for _, bad := range []string{"epc1.@@", enc[:len(enc)-5], "xyz"} {
		_, err := DecodeBundle(bad)
		if err == nil {
			t.Errorf("accepted %q", bad)
			continue
		}
		if strings.Contains(err.Error(), full) || strings.Contains(err.Error(), "epc1.") && len(err.Error()) > 80 {
			t.Errorf("error leaks content: %v", err)
		}
	}
	noCA := b
	noCA.CAs = nil
	e2, _ := noCA.Encode()
	if _, err := DecodeBundle(e2); err == nil {
		t.Error("bundle without a CA accepted")
	}
}

func TestVPNBundle(t *testing.T) {
	key := "hskey-auth-abcdefghijklmnopqrstuvwxyz0123456789"
	enc, err := VPNBundle{Server: "https://vpn.example.com", Key: key}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeVPNBundle(enc); err != nil || got.Key != key {
		t.Fatalf("decode: %v", err)
	}
	if _, err := DecodeVPNBundle(enc[:len(enc)-4]); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("corrupted bundle: %v", err)
	}
	if _, err := (VPNBundle{Server: "http://vpn.example.com", Key: key}).Encode(); err != nil {
		t.Fatal(err)
	}
	plain, _ := VPNBundle{Server: "http://vpn.example.com", Key: key}.Encode()
	if _, err := DecodeVPNBundle(plain); err == nil {
		t.Fatal("plain HTTP control server accepted")
	}
}
