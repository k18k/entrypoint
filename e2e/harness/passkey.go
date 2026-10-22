package harness

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// Passkey is a software FIDO2 authenticator for tests: one discoverable P-256 credential,
// "none" attestation, user presence and verification always given.
type Passkey struct {
	Origin     string
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	rpID       string
	count      uint32
}

var b64 = base64.RawURLEncoding

// Create answers navigator.credentials.create() options (the "publicKey" member) and returns the
// JSON body the server's FinishRegistration expects.
func (p *Passkey) Create(options json.RawMessage) ([]byte, error) {
	var o struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	p.key, p.rpID = key, o.RP.ID
	p.credID = make([]byte, 32)
	rand.Read(p.credID)
	if p.userHandle, err = b64.DecodeString(o.User.ID); err != nil {
		return nil, fmt.Errorf("user.id: %w", err)
	}
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": o.Challenge, "origin": p.Origin, "crossOrigin": false})
	pub, _ := key.PublicKey.ECDH()
	raw := pub.Bytes() // 0x04 || X || Y
	cose := cborMap(
		cborInt(1), cborInt(2), // kty: EC2
		cborInt(3), cborInt(-7), // alg: ES256
		cborInt(-1), cborInt(1), // crv: P-256
		cborInt(-2), cborBytes(raw[1:33]),
		cborInt(-3), cborBytes(raw[33:65]),
	)
	att := make([]byte, 16, 16+2+len(p.credID)+len(cose)) // AAGUID 0
	att = binary.BigEndian.AppendUint16(att, uint16(len(p.credID)))
	att = append(att, p.credID...)
	att = append(att, cose...)
	authData := p.authData(0x01|0x04|0x40, att)
	ao := cborMap(cborText("fmt"), cborText("none"), cborText("attStmt"), cborMap(), cborText("authData"), cborBytes(authData))
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(p.credID), "rawId": b64.EncodeToString(p.credID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64.EncodeToString(cdj), "attestationObject": b64.EncodeToString(ao)},
	})
}

// Get answers navigator.credentials.get() options and returns the body for FinishPasskeyLogin.
func (p *Passkey) Get(options json.RawMessage) ([]byte, error) {
	var o struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		return nil, err
	}
	if p.key == nil {
		// Never registered: sign with a throwaway credential, like a key from another site.
		p.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		p.credID, p.userHandle = make([]byte, 32), make([]byte, 32)
		rand.Read(p.credID)
		rand.Read(p.userHandle)
		p.rpID = "panel.example.com"
	}
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": o.Challenge, "origin": p.Origin, "crossOrigin": false})
	authData := p.authData(0x01|0x04, nil)
	h := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, authData...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, p.key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": b64.EncodeToString(p.credID), "rawId": b64.EncodeToString(p.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cdj), "authenticatorData": b64.EncodeToString(authData),
			"signature": b64.EncodeToString(sig), "userHandle": b64.EncodeToString(p.userHandle),
		},
	})
}

func (p *Passkey) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte(p.rpID))
	p.count++
	out := append(rp[:], flags)
	out = binary.BigEndian.AppendUint32(out, p.count)
	return append(out, attested...)
}

// Minimal CBOR (RFC 8949) for the attestation object and the COSE key.
func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
	default:
		return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(n))
	}
}

func cborInt(v int) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}
func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }
func cborMap(kv ...[]byte) []byte {
	out := cborHead(5, uint64(len(kv)/2))
	for _, x := range kv {
		out = append(out, x...)
	}
	return out
}
