package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Strix vuln-0005: the minimum passphrase length applies to --password-file too.
func TestPasswordFileMinLength(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pw")
	os.WriteFile(f, []byte("short\n"), 0o600)
	if _, err := passwordFrom(f, true); err == nil {
		t.Fatal("short passphrase accepted for backup")
	}
	if _, err := passwordFrom(f, false); err != nil {
		t.Fatalf("restore with existing short passphrase refused: %v", err)
	}
}
