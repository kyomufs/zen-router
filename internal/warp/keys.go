package warp

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// Key is a WireGuard Curve25519 keypair, base64-encoded the same way the
// official clients and wgcf store them.
type Key struct {
	Private string // base64, 32 bytes
	Public  string // base64, 32 bytes
}

// NewKeyPair generates a fresh WireGuard keypair. WireGuard stores the raw
// 32-byte scalar as the private key (clamping happens at scalar-mult time),
// which is exactly what crypto/ecdh's X25519 implementation exposes.
func NewKeyPair() (*Key, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate x25519 key: %w", err)
	}
	return &Key{
		Private: base64.StdEncoding.EncodeToString(priv.Bytes()),
		Public:  base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()),
	}, nil
}

// IsKey reports whether s looks like a base64 WireGuard key (32 bytes).
func IsKey(s string) bool {
	raw, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(raw) == 32
}
