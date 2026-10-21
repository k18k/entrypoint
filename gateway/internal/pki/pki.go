// Package pki handles the gateway's private CA and the tunnel port certificate.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

const (
	CAValidity     = 10 * 365 * 24 * time.Hour
	ServerValidity = 365 * 24 * time.Hour
	RenewBefore    = 30 * 24 * time.Hour
)

// CA is the authority's certificate and key.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *ecdsa.PrivateKey
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA creates a new CA.
func NewCA(name string) (*CA, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: name, Organization: []string{"entrypoint"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return &CA{Cert: cert, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Key: key}, keyPEM, nil
}

// IssueServer issues a server certificate for the gateway's DNS name.
func (ca *CA) IssueServer(dnsName string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	notAfter := now.Add(ServerValidity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// LoadCA loads a CA from files.
func LoadCA(certFile, keyFile string) (*CA, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	cert, err := ParseCert(certPEM)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("invalid CA key file")
	}
	key, err := x509.ParseECPrivateKey(b.Bytes)
	if err != nil {
		return nil, errors.New("invalid CA key")
	}
	return &CA{Cert: cert, CertPEM: certPEM, Key: key}, nil
}

// ParseCert parses the first PEM certificate.
func ParseCert(p []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(p)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// Fingerprint returns a shortened SHA-256 fingerprint of the certificate (16 hex characters).
func Fingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:8])
}

// FingerprintPEM computes the fingerprint of a PEM certificate.
func FingerprintPEM(p []byte) (string, error) {
	c, err := ParseCert(p)
	if err != nil {
		return "", err
	}
	return Fingerprint(c), nil
}

// NeedsRenewal reports whether the server certificate needs renewing.
func NeedsRenewal(certPEM []byte, now time.Time) (bool, error) {
	c, err := ParseCert(certPEM)
	if err != nil {
		return true, err
	}
	return now.Add(RenewBefore).After(c.NotAfter), nil
}

// IssuedBy reports whether the certificate was issued by the given CA.
func IssuedBy(certPEM []byte, ca *x509.Certificate) bool {
	c, err := ParseCert(certPEM)
	if err != nil {
		return false
	}
	return c.CheckSignatureFrom(ca) == nil
}

// WriteFileAtomic writes a file via a temporary file, fsync and an atomic rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dirOf(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			if i == 0 {
				return "/"
			}
			return p[:i]
		}
	}
	return "."
}
