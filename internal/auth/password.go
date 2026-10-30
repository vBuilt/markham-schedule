// Package auth holds the building blocks for public accounts: password and
// token hashing, rate limiting, outbound mail and bot checks.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory  = 19456
	argonTime    = 2
	argonThreads = 1
	argonSaltLen = 16
	argonKeyLen  = 32

	// argonSlots caps concurrent argon2 runs; each takes argonMemory KiB.
	argonSlots = 4
)

var (
	argonSem = make(chan struct{}, argonSlots)
	// argonKey derives the key; tests replace it to observe calls.
	argonKey = argon2.IDKey
)

// deriveKey runs argon2id, waiting for a free slot so that at most argonSlots
// derivations (and their memory) are in use at once.
func deriveKey(pw, salt []byte, t, m uint32, p uint8, keyLen uint32) []byte {
	argonSem <- struct{}{}
	defer func() { <-argonSem }()
	return argonKey(pw, salt, t, m, p, keyLen)
}

// HashPassword returns an argon2id hash as a PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := deriveKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches the PHC string; false if malformed.
func CheckPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	// Guard against absurd parameters in a corrupted or hostile string.
	if m == 0 || m > 1<<20 || t == 0 || t > 16 || p == 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := deriveKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
