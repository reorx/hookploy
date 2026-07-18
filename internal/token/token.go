// Package token generates and hashes hookploy tokens. Plaintext tokens are
// shown once at creation; only SHA-256 hashes are stored.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
)

// Kind is the token class, which determines its prefix and privileges.
type Kind string

const (
	KindService Kind = "service" // hpt_: may trigger deploys of one service
	KindServer  Kind = "server"  // hps_: edge self-identification (M2)
	KindAdmin   Kind = "admin"   // hpa_: read status API, trigger deploys/tasks
)

func (k Kind) prefix() string {
	switch k {
	case KindService:
		return "hpt_"
	case KindServer:
		return "hps_"
	case KindAdmin:
		return "hpa_"
	}
	panic("unknown token kind: " + string(k))
}

// New returns a fresh plaintext token: prefix + 43 chars base64url (32 bytes).
func New(kind Kind) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return kind.prefix() + base64.RawURLEncoding.EncodeToString(b[:])
}

// Hash returns the hex SHA-256 of a plaintext token, the stored form.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Equal compares two hashes in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
