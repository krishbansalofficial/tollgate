// Package auth implements virtual API keys: generation, hashing, lookup with
// an in-process TTL cache, and the HTTP middleware that enforces them.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"strings"
)

const (
	keyPrefix    = "tg_"
	displayChars = 8
	maxKeyLen    = 64 // tg_ + ~43 base62 chars; anything longer is garbage
)

// Generate returns a new raw key (shown to the user once), its hash (stored),
// and a short display prefix (stored in plain text).
//
// 32 bytes from crypto/rand = 256 bits of entropy, rendered in base62 so the
// key is copy-paste and URL safe. big.Int.Text supports bases up to 62.
func Generate() (raw string, hash [32]byte, display string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", hash, "", err
	}
	body := new(big.Int).SetBytes(b[:]).Text(62)
	raw = keyPrefix + body
	return raw, Hash(raw), keyPrefix + body[:displayChars], nil
}

// Hash is SHA-256, deliberately not bcrypt/argon2. Slow hashes exist because
// passwords are low-entropy and guessable; a 256-bit random key is not, so a
// fast hash is equally safe and keeps auth off the latency budget.
func Hash(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }

// wellFormed rejects obviously invalid keys before any cache or DB work, so
// garbage headers can't pollute the negative cache.
func wellFormed(raw string) bool {
	return strings.HasPrefix(raw, keyPrefix) && len(raw) > len(keyPrefix)+displayChars && len(raw) <= maxKeyLen
}
