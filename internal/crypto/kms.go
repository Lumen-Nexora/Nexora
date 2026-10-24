package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"
)

// KMS envelope encryption for custodial wallet seeds (#97).
//
// A sealed secret is a "v2" envelope:
//
//	v2::<provider>::<base64 wrapped DEK>::<hex nonce||ciphertext>
//
// Every secret gets its own random 256-bit data encryption key (DEK). The DEK
// encrypts the secret with AES-256-GCM and is then wrapped (encrypted) by a
// key encryption key (KEK) that never leaves AWS KMS, Google Cloud KMS or
// HashiCorp Vault. Only the wrapped DEK is stored. Opening a secret unwraps
// the DEK through the provider, decrypts into a short-lived buffer, hands it
// to the caller and overwrites both DEK and plaintext with zeros as soon as
// the caller returns.
//
// Each provider embeds the KEK version in its ciphertext, so rotating the KEK
// never breaks existing envelopes; RewrapSecret moves one onto the current
// version without touching the encrypted secret.

const (
	envelopeV2      = 2
	dekSize         = 32
	kmsCallTimeout  = 10 * time.Second
	v2FieldSep      = "::"
	v2Prefix        = "v2" + v2FieldSep
	maxProviderName = 32
)

var (
	ErrKMSNotConfigured    = errors.New("envelope was sealed with a KMS but no KMS is configured")
	ErrKMSProviderMismatch = errors.New("envelope was sealed by a different KMS provider")
	ErrKMSRequest          = errors.New("kms request failed")
	ErrInvalidKMSConfig    = errors.New("invalid kms configuration")
)

// KMS wraps and unwraps data encryption keys with a key encryption key held
// by an external key manager.
type KMS interface {
	// Name identifies the provider in the envelope, e.g. "aws-kms".
	Name() string
	// Wrap encrypts a DEK under the current KEK version.
	Wrap(ctx context.Context, dek []byte) ([]byte, error)
	// Unwrap decrypts a wrapped DEK, under whichever KEK version wrapped it.
	Unwrap(ctx context.Context, wrapped []byte) ([]byte, error)
	// Rewrap re-encrypts a wrapped DEK under the current KEK version.
	Rewrap(ctx context.Context, wrapped []byte) ([]byte, error)
	// EnsureRotation applies an automatic rotation policy to the KEK; the
	// provider then rotates it on schedule.
	EnsureRotation(ctx context.Context, policy RotationPolicy) error
}

// RotationPolicy is the automatic KEK rotation schedule enforced on the
// provider. Providers clamp or reject periods outside their limits.
type RotationPolicy struct {
	Period time.Duration
}

var (
	kmsMu     sync.RWMutex
	activeKMS KMS
)

// UseKMS installs the process-wide key manager used to seal new secrets and
// open v2 envelopes. Passing nil removes it.
func UseKMS(k KMS) {
	kmsMu.Lock()
	defer kmsMu.Unlock()
	activeKMS = k
}

// CurrentKMS returns the installed key manager, or nil.
func CurrentKMS() KMS {
	kmsMu.RLock()
	defer kmsMu.RUnlock()
	return activeKMS
}

// Zeroize overwrites b with zeros. The KeepAlive keeps the compiler from
// treating the writes as dead stores.
func Zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
	runtime.KeepAlive(b)
}

// SealSecret encrypts a secret for storage. With a KMS installed it produces a
// v2 envelope under a fresh DEK; without one it falls back to the v1
// master-key envelope, so deployments without a KMS are unchanged.
func SealSecret(ctx context.Context, plaintext, legacyKey []byte) ([]byte, error) {
	k := CurrentKMS()
	if k == nil {
		return Encrypt(plaintext, legacyKey)
	}
	return sealV2(ctx, k, plaintext)
}

// OpenSecret decrypts a stored secret into an ephemeral buffer, calls fn with
// it and zeroizes the buffer (and, for v2, the DEK) before returning,
// whatever fn does. fn must not retain the slice. v1 and legacy envelopes are
// opened with legacyKey.
func OpenSecret(ctx context.Context, envelope, legacyKey []byte, fn func(plaintext []byte) error) error {
	var plaintext []byte
	var err error
	if isV2(envelope) {
		plaintext, err = openV2(ctx, CurrentKMS(), envelope)
	} else {
		plaintext, err = Decrypt(envelope, legacyKey)
	}
	if err != nil {
		return err
	}
	defer Zeroize(plaintext)
	return fn(plaintext)
}

// RewrapSecret brings a stored secret onto the current KEK version. A v2
// envelope only has its DEK re-wrapped; the encrypted secret is unchanged and
// the secret itself is never decrypted. A v1 or legacy envelope is migrated
// to v2, decrypting it only transiently.
func RewrapSecret(ctx context.Context, envelope, legacyKey []byte) ([]byte, error) {
	k := CurrentKMS()
	if k == nil {
		return nil, ErrKMSNotConfigured
	}
	if !isV2(envelope) {
		var sealed []byte
		err := OpenSecret(ctx, envelope, legacyKey, func(plaintext []byte) error {
			var sealErr error
			sealed, sealErr = sealV2(ctx, k, plaintext)
			return sealErr
		})
		return sealed, err
	}
	env, err := parseV2(envelope)
	if err != nil {
		return nil, err
	}
	if env.provider != k.Name() {
		return nil, ErrKMSProviderMismatch
	}
	ctx, cancel := context.WithTimeout(ctx, kmsCallTimeout)
	defer cancel()
	rewrapped, err := k.Rewrap(ctx, env.wrappedDEK)
	if err != nil {
		return nil, err
	}
	env.wrappedDEK = rewrapped
	return env.marshal(), nil
}

// v2Envelope is the parsed form of a v2 envelope.
type v2Envelope struct {
	provider   string
	wrappedDEK []byte
	nonce      []byte
	ciphertext []byte
}

func isV2(envelope []byte) bool {
	return len(envelope) >= len(v2Prefix) && string(envelope[:len(v2Prefix)]) == v2Prefix
}

func (e *v2Envelope) marshal() []byte {
	payload := append(append([]byte{}, e.nonce...), e.ciphertext...)
	return []byte(v2Prefix + e.provider + v2FieldSep +
		base64.StdEncoding.EncodeToString(e.wrappedDEK) + v2FieldSep + hexEncode(payload))
}

// aad binds the ciphertext to the envelope version and provider, so a payload
// cannot be replayed under a different header.
func (e *v2Envelope) aad() []byte { return []byte(v2Prefix + e.provider) }

func parseV2(data []byte) (*v2Envelope, error) {
	parts := strings.Split(string(data), v2FieldSep)
	if len(parts) != 4 || parts[0] != "v2" || parts[1] == "" || len(parts[1]) > maxProviderName {
		return nil, ErrInvalidCiphertext
	}
	wrapped, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil || len(wrapped) == 0 {
		return nil, ErrInvalidCiphertext
	}
	payload, err := hexDecode(parts[3])
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	gcm, err := newGCM(make([]byte, dekSize))
	if err != nil {
		return nil, err
	}
	if len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	return &v2Envelope{
		provider:   parts[1],
		wrappedDEK: wrapped,
		nonce:      payload[:gcm.NonceSize()],
		ciphertext: payload[gcm.NonceSize():],
	}, nil
}

func sealV2(ctx context.Context, k KMS, plaintext []byte) ([]byte, error) {
	dek := make([]byte, dekSize)
	defer Zeroize(dek)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("generate data key: %w", err)
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	env := &v2Envelope{provider: k.Name(), nonce: make([]byte, gcm.NonceSize())}
	if _, err := io.ReadFull(rand.Reader, env.nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	env.ciphertext = gcm.Seal(nil, env.nonce, plaintext, env.aad())

	ctx, cancel := context.WithTimeout(ctx, kmsCallTimeout)
	defer cancel()
	if env.wrappedDEK, err = k.Wrap(ctx, dek); err != nil {
		return nil, fmt.Errorf("wrap data key: %w", err)
	}
	return env.marshal(), nil
}

func openV2(ctx context.Context, k KMS, envelope []byte) ([]byte, error) {
	if k == nil {
		return nil, ErrKMSNotConfigured
	}
	env, err := parseV2(envelope)
	if err != nil {
		return nil, err
	}
	if env.provider != k.Name() {
		return nil, fmt.Errorf("%w: sealed by %s, configured %s", ErrKMSProviderMismatch, env.provider, k.Name())
	}
	ctx, cancel := context.WithTimeout(ctx, kmsCallTimeout)
	defer cancel()
	dek, err := k.Unwrap(ctx, env.wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("unwrap data key: %w", err)
	}
	defer Zeroize(dek)
	if len(dek) != dekSize {
		return nil, ErrDecryptionFailed
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, env.nonce, env.ciphertext, env.aad())
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// KMSConfig selects and configures the key manager. Provider is "" or
// "none" (no KMS), "aws", "gcp" or "vault".
type KMSConfig struct {
	Provider       string
	RotationPeriod time.Duration
	AWS            AWSKMSConfig
	GCP            GCPKMSConfig
	Vault          VaultConfig
}

// NewKMS builds the configured key manager, or returns nil when none is
// configured.
func NewKMS(cfg KMSConfig) (KMS, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return nil, nil
	case "aws":
		return NewAWSKMS(cfg.AWS)
	case "gcp":
		return NewGCPKMS(cfg.GCP)
	case "vault":
		return NewVaultTransit(cfg.Vault)
	default:
		return nil, fmt.Errorf("%w: unknown provider %q (want aws, gcp, vault or none)", ErrInvalidKMSConfig, cfg.Provider)
	}
}

// ConfigureKMS builds the configured key manager, applies the rotation
// policy when one is set, and installs it process-wide. It returns nil when
// no KMS is configured.
func ConfigureKMS(ctx context.Context, cfg KMSConfig) (KMS, error) {
	k, err := NewKMS(cfg)
	if err != nil || k == nil {
		return nil, err
	}
	if cfg.RotationPeriod > 0 {
		rctx, cancel := context.WithTimeout(ctx, kmsCallTimeout)
		defer cancel()
		if err := k.EnsureRotation(rctx, RotationPolicy{Period: cfg.RotationPeriod}); err != nil {
			return nil, fmt.Errorf("apply %s rotation policy: %w", k.Name(), err)
		}
	}
	UseKMS(k)
	return k, nil
}
