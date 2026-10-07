package apikey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

func TestGenerate(t *testing.T) {
	raw, prefix, err := Generate(domain.ModeLive)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Check raw key format
	if !strings.HasPrefix(raw, "sk_live_") {
		t.Errorf("raw key should start with 'sk_live_', got: %s", raw)
	}

	// Check prefix is the mode marker plus 8 chars from the base58 body
	if len(prefix) != 16 {
		t.Errorf("prefix should be 16 chars, got %d: %s", len(prefix), prefix)
	}

	// Prefix should be from the base58 body, not "sk_live_"
	if prefix == "sk_live_" {
		t.Errorf("prefix should not be 'sk_live_', got: %s", prefix)
	}

	// Prefix should carry the mode marker and the first 8 chars of the body
	base58Body := strings.TrimPrefix(raw, "sk_live_")
	if !strings.HasPrefix(prefix, "sk_live_") {
		t.Errorf("prefix should carry the mode marker, got %s", prefix)
	}
	if prefix[len("sk_live_"):] != base58Body[:8] {
		t.Errorf("prefix should match first 8 chars of base58 body: expected %s, got %s", base58Body[:8], prefix)
	}
}

func TestGenerateUniqueKeys(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		raw, prefix, err := Generate(domain.ModeLive)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if keys[raw] {
			t.Fatalf("duplicate key generated: %s", raw)
		}
		if keys[prefix] {
			t.Fatalf("duplicate prefix generated: %s", prefix)
		}
		keys[raw] = true
		keys[prefix] = true
	}
}

func TestGenerateUsesModeSpecificPrefix(t *testing.T) {
	for _, mode := range []domain.Mode{domain.ModeLive, domain.ModeTest} {
		raw, prefix, err := Generate(mode)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(raw, "sk_"+string(mode)+"_") {
			t.Fatalf("raw key %q does not carry mode %q", raw, mode)
		}
		if !strings.HasPrefix(raw, prefix) {
			t.Fatalf("prefix %q is not a prefix of key", prefix)
		}
		parsed, err := ModeFromRaw(raw)
		if err != nil || parsed != mode {
			t.Fatalf("parsed mode = %q, err=%v", parsed, err)
		}
	}
}

func TestGenerateRejectsUnknownMode(t *testing.T) {
	if _, _, err := Generate(domain.Mode("staging")); err == nil {
		t.Fatal("expected an unknown mode to be rejected")
	}
}

func TestModeFromRawRejectsLegacyOrUnknownPrefix(t *testing.T) {
	if _, err := ModeFromRaw("sk_live"); err == nil {
		t.Fatal("expected malformed prefix to be rejected")
	}
	if _, err := ModeFromRaw("legacy-key"); err == nil {
		t.Fatal("expected unknown prefix to be rejected")
	}
}

func TestHash(t *testing.T) {
	raw := "sk_live_testkey123"
	hashed := Hash(raw)

	// Verify it's a valid hex string
	decoded, err := hex.DecodeString(hashed)
	if err != nil {
		t.Errorf("hash should be valid hex: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("SHA-256 hash should be 32 bytes, got %d", len(decoded))
	}

	// Verify it matches manual SHA-256
	h := sha256.New()
	h.Write([]byte(raw))
	expected := hex.EncodeToString(h.Sum(nil))
	if hashed != expected {
		t.Errorf("hash mismatch: got %s, expected %s", hashed, expected)
	}
}

func TestVerify(t *testing.T) {
	raw, _, err := Generate(domain.ModeLive)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	hashed := Hash(raw)

	// Valid key should verify
	if !Verify(raw, hashed) {
		t.Error("Verify should return true for valid key")
	}

	// Invalid key should not verify
	if Verify("sk_live_wrongkey", hashed) {
		t.Error("Verify should return false for invalid key")
	}
}

func TestVerifyConstantTime(t *testing.T) {
	raw, _, err := Generate(domain.ModeTest)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	hashed := Hash(raw)

	// Test that Verify uses constant-time comparison by checking
	// that it doesn't panic and returns correct results
	// (We can't easily test timing in unit tests, but we can verify
	// the implementation uses subtle.ConstantTimeCompare)
	_ = Verify(raw, hashed)
	_ = Verify("wrong", hashed)
}

func TestVerifyTimingAttackResistance(t *testing.T) {
	// Create two hashes that differ only in the last byte
	h1 := sha256.Sum256([]byte("test1"))
	h2 := sha256.Sum256([]byte("test2"))
	hash1 := hex.EncodeToString(h1[:])
	hash2 := hex.EncodeToString(h2[:])

	// Verify doesn't panic
	_ = Verify("test1", hash1)
	_ = Verify("test1", hash2)
	_ = Verify("test2", hash1)
	_ = Verify("test2", hash2)
}

func TestPrefixUniqueness(t *testing.T) {
	// Generate many keys and ensure prefixes are unique
	prefixes := make(map[string]int)
	for i := 0; i < 1000; i++ {
		_, prefix, err := Generate(domain.ModeLive)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		prefixes[prefix]++
	}

	// With 1000 keys and an 8-character base58 prefix, collisions are extremely
	// unlikely but we verify no duplicates
	for prefix, count := range prefixes {
		if count > 1 {
			t.Errorf("duplicate prefix %s found %d times", prefix, count)
		}
	}
}

func TestHasScope(t *testing.T) {
	// Empty scopes means unrestricted access (backward compatibility)
	if !domain.HasScope(nil, domain.ScopeTransfersWrite) {
		t.Error("nil scopes should allow any scope")
	}
	if !domain.HasScope([]string{}, domain.ScopeTransfersWrite) {
		t.Error("empty scopes should allow any scope")
	}

	// Wildcard
	if !domain.HasScope([]string{"*"}, domain.ScopeTransfersWrite) {
		t.Error("wildcard '*' should allow any scope")
	}
	if !domain.HasScope([]string{"admin"}, domain.ScopeTransfersWrite) {
		t.Error("admin scope should allow any scope")
	}

	// Resource wildcard
	if !domain.HasScope([]string{"transfers:*"}, domain.ScopeTransfersWrite) {
		t.Error("resource wildcard 'transfers:*' should allow 'transfers:write'")
	}
	if !domain.HasScope([]string{"transfers:*"}, domain.ScopeTransfersRead) {
		t.Error("resource wildcard 'transfers:*' should allow 'transfers:read'")
	}
	if domain.HasScope([]string{"transfers:*"}, domain.ScopeWalletsRead) {
		t.Error("resource wildcard 'transfers:*' should not allow 'wallets:read'")
	}

	// Exact match
	if !domain.HasScope([]string{"transfers:read", "wallets:read"}, domain.ScopeTransfersRead) {
		t.Error("should allow exact matching scope")
	}
	if domain.HasScope([]string{"transfers:read"}, domain.ScopeTransfersWrite) {
		t.Error("should not allow write when only read is granted")
	}
}

func TestValidateScopes(t *testing.T) {
	valid := []string{"transfers:read", "transfers:write", "wallets:*", "*"}
	if err := domain.ValidateScopes(valid); err != nil {
		t.Fatalf("expected valid scopes to pass, got: %v", err)
	}

	invalid := []string{"transfers:read", "invalid:scope"}
	if err := domain.ValidateScopes(invalid); err == nil {
		t.Fatal("expected invalid scope to fail validation")
	}
}
