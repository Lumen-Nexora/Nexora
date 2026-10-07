package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

var b58Alphabet = []byte("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz")

func base58Encode(b []byte) string {
	x := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := &big.Int{}
	var result []byte
	for x.Cmp(zero) > 0 {
		x.DivMod(x, base, mod)
		result = append(result, b58Alphabet[mod.Int64()])
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	for _, byteVal := range b {
		if byteVal == 0x00 {
			result = append([]byte{b58Alphabet[0]}, result...)
		} else {
			break
		}
	}
	return string(result)
}

// Generate creates a new API key of the form sk_<mode>_<base58>. The mode is
// carried by the raw key so a credential cannot be replayed against the other
// environment: sk_live_ keys authenticate against mainnet and sk_test_ keys
// against the isolated testnet tenant environment.
func Generate(mode domain.Mode) (raw string, prefix string, err error) {
	if !mode.Valid() {
		return "", "", fmt.Errorf("invalid API key mode %q", mode)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = "sk_" + string(mode) + "_" + base58Encode(b)

	// Prefix is the environment marker plus the first eight base58 characters,
	// long enough to identify a key in a list without being usable as one.
	prefix = raw[:16]
	return raw, prefix, nil
}

// ModeFromRaw returns the environment encoded by a raw API key. It is used by
// authentication to reject keys minted for the other environment rather than
// silently upgrading them.
func ModeFromRaw(raw string) (domain.Mode, error) {
	switch {
	case strings.HasPrefix(raw, "sk_live_"):
		return domain.ModeLive, nil
	case strings.HasPrefix(raw, "sk_test_"):
		return domain.ModeTest, nil
	default:
		return "", fmt.Errorf("API key must start with sk_live_ or sk_test_")
	}
}

// Hash returns the SHA-256 hash of the raw API key for storage
func Hash(raw string) string {
	h := sha256.New()
	h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks if the provided raw key matches the stored hash using constant-time comparison
func Verify(raw, hashed string) bool {
	computed := Hash(raw)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(hashed)) == 1
}
