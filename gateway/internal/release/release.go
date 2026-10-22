// Package release verifies and unpacks Entrypoint releases: SHA256SUMS signed with Ed25519
// (SHA256SUMS.sig, the raw signature in base64), and the gateway tarball listed in it.
package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// PublicKeys sign official releases (base64 of the raw 32-byte Ed25519 key).
var PublicKeys = []string{
	"8reBNWMRtX615Ph1b1Z0LXRD9jvCjqgO4gOFq+N8eqk=",
}

// VerifySums checks the signature of a SHA256SUMS file against the official keys and any extra
// trusted keys (tests), and returns the listed sums by file name.
func VerifySums(sums, sig []byte, extraKeys []string) (map[string]string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("SHA256SUMS.sig: not an Ed25519 signature")
	}
	ok := false
	for _, k := range append(append([]string{}, PublicKeys...), extraKeys...) {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, sums, raw) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, errors.New("SHA256SUMS: the signature does not match any trusted release key")
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(sums), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = f[0]
		}
	}
	return out, nil
}

// CheckFile compares a file with its sum.
func CheckFile(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: wrong SHA256 (%s, expected %s)", filepath.Base(path), got, want)
	}
	return nil
}

// Extract unpacks the gateway tarball into dest, dropping its top directory
// (entrypoint_<version>_<platform>/). Only regular files and directories inside dest are allowed.
func Extract(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root := filepath.Clean(dest) + string(filepath.Separator)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("corrupted tarball: %w", err)
		}
		_, rel, _ := strings.Cut(strings.TrimPrefix(filepath.ToSlash(h.Name), "./"), "/")
		if rel == "" {
			continue
		}
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if !strings.HasPrefix(p, root) {
			return fmt.Errorf("path outside the release directory: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(h.Mode).Perm()&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("file type not allowed in the tarball: %s", h.Name)
		}
	}
}
