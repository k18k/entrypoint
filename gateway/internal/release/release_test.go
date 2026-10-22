package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifySums(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	k := base64.StdEncoding.EncodeToString(pub)
	sums := []byte(strings.Repeat("a", 64) + "  entrypoint_1.0.0_linux_amd64.tar.gz\n")
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)))
	got, err := VerifySums(sums, sig, []string{k})
	if err != nil || got["entrypoint_1.0.0_linux_amd64.tar.gz"] == "" {
		t.Fatalf("valid signature: %v %v", got, err)
	}
	if _, err := VerifySums(sums, sig, nil); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := VerifySums(append(sums, 'x'), sig, []string{k}); err == nil {
		t.Fatal("modified SHA256SUMS accepted")
	}
}

// The release workflow signs with openssl; the gateway verifies with crypto/ed25519.
func TestOpenSSLSignature(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k.pem")
	sums := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, []byte(strings.Repeat("b", 64)+"  x\n"), 0o644)
	for _, c := range [][]string{
		{"genpkey", "-algorithm", "ed25519", "-out", key},
		{"pkeyutl", "-sign", "-inkey", key, "-rawin", "-in", sums, "-out", sums + ".raw"},
		{"pkey", "-in", key, "-pubout", "-outform", "DER", "-out", key + ".pub"},
	} {
		if out, err := exec.Command("openssl", c...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v %s", c, err, out)
		}
	}
	der, _ := os.ReadFile(key + ".pub")
	raw, _ := os.ReadFile(sums + ".raw")
	body, _ := os.ReadFile(sums)
	pub := base64.StdEncoding.EncodeToString(der[len(der)-32:])
	if _, err := VerifySums(body, []byte(base64.StdEncoding.EncodeToString(raw)), []string{pub}); err != nil {
		t.Fatal(err)
	}
}
