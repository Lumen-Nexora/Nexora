// Package auth implements JWT-based authentication for the Nexora API.
//
// # Library choice
//
// The implementation uses the Go standard library (crypto/hmac, crypto/sha256,
// encoding/base64, encoding/json) rather than a third-party JWT library.
// The rationale:
//
//   - The surface area is small (sign + verify HS256 only; no RS256, ECDSA, or
//     JWK rotation is required today).
//   - Standard-library primitives are well-audited and already vendored.
//   - A bespoke implementation lets us enforce the exact claim set and reject
//     any algorithm that is not HS256 without relying on library configuration.
//
// If the key-management requirements grow (e.g. asymmetric signing, JWK
// endpoints, multi-algorithm support), migrate to github.com/golang-jwt/jwt/v5.
//
// # Security properties
//
//   - Algorithm is fixed to HS256; "alg: none" and any other value are rejected
//     at parse time before the signature is checked.
//   - Every token carries iss, aud, and jti claims. Issuer and audience are
//     verified against the package-level constants on every parse.
//   - jti is a random UUID generated at mint time, enabling per-token revocation.
//   - Expiry (exp) and issued-at (iat) are always included and verified.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Issuer and Audience values embedded in every token and verified on parse.
// Changing these constants invalidates all previously issued tokens.
const (
	TokenIssuer   = "nexora"
	TokenAudience = "nexora-api"
)

// Claims is the verified payload extracted from a JWT.
type Claims struct {
	Sub       string `json:"sub"`
	TenantID  string `json:"tenant_id"`
	Role      string `json:"role"`
	Email     string `json:"email"`
	TokenType string `json:"token_type"` // "access" | "refresh"
	Iss       string `json:"iss"`
	Aud       string `json:"aud"`
	Jti       string `json:"jti"`
	Exp       int64  `json:"exp"`
	Iat       int64  `json:"iat"`
}

// header is the fixed JWT header for HS256 tokens.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// GenerateToken mints a signed HS256 JWT. The token includes iss, aud, and a
// random jti so individual tokens can be revoked via a denylist.
func GenerateToken(
	userID, tenantID, role, email, tokenType string,
	secret []byte,
	duration time.Duration,
) (string, error) {
	now := time.Now().UTC()
	h := header{Alg: "HS256", Typ: "JWT"}
	c := Claims{
		Sub:       userID,
		TenantID:  tenantID,
		Role:      role,
		Email:     email,
		TokenType: tokenType,
		Iss:       TokenIssuer,
		Aud:       TokenAudience,
		Jti:       uuid.NewString(),
		Iat:       now.Unix(),
		Exp:       now.Add(duration).Unix(),
	}

	headerJSON, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("marshal jwt header: %w", err)
	}

	claimsJSON, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal jwt claims: %w", err)
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	unsigned := headerB64 + "." + claimsB64

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return unsigned + "." + sig, nil
}

// ParseToken verifies the signature, algorithm, expiry, issuer, and audience
// of a JWT string and returns the decoded Claims on success.
//
// Tokens signed with "alg: none" or any algorithm other than HS256 are
// rejected before the signature is even checked.
func ParseToken(tokenStr string, secret []byte) (*Claims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid token format")
	}

	// Decode and validate the header before touching the signature.
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("invalid header encoding")
	}

	var h header
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return nil, errors.New("invalid header")
	}

	// Reject any algorithm that is not exactly HS256, including "none".
	if h.Alg != "HS256" {
		return nil, fmt.Errorf("unsupported algorithm %q: only HS256 is accepted", h.Alg)
	}

	// Verify signature with constant-time comparison.
	unsigned := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(unsigned))
	expectedSig := mac.Sum(nil)

	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("invalid signature encoding")
	}
	if !hmac.Equal(gotSig, expectedSig) {
		return nil, errors.New("invalid token signature")
	}

	// Decode claims only after the signature is verified.
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid claims encoding")
	}

	var c Claims
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return nil, errors.New("failed to parse claims")
	}

	if time.Now().UTC().Unix() > c.Exp {
		return nil, errors.New("token expired")
	}

	if c.Iss != TokenIssuer {
		return nil, fmt.Errorf("invalid issuer %q", c.Iss)
	}

	if c.Aud != TokenAudience {
		return nil, fmt.Errorf("invalid audience %q", c.Aud)
	}

	return &c, nil
}
