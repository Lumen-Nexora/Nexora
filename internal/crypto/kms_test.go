package crypto

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyRing is a versioned KEK, as each provider keeps one. Encrypt always uses
// the newest version; decrypt uses the version recorded in the ciphertext.
type keyRing struct {
	mu       sync.Mutex
	versions [][]byte
	// lastDecryptVersion is the KEK version used by the latest decrypt.
	lastDecryptVersion int
}

func newKeyRing() *keyRing {
	r := &keyRing{}
	r.rotate()
	return r
}

func (r *keyRing) rotate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	r.versions = append(r.versions, key)
}

func (r *keyRing) encrypt(plaintext []byte) (int, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	version := len(r.versions)
	gcm := ringGCM(r.versions[version-1])
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return version, append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...)
}

func (r *keyRing) decrypt(version int, sealed []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if version < 1 || version > len(r.versions) {
		return nil, errors.New("unknown key version")
	}
	r.lastDecryptVersion = version
	gcm := ringGCM(r.versions[version-1])
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("short ciphertext")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func ringGCM(key []byte) cipher.AEAD {
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	return gcm
}

// blob encodes a ciphertext with its version, as AWS and GCP do opaquely.
func blob(version int, sealed []byte) []byte {
	return append([]byte(fmt.Sprintf("v%d|", version)), sealed...)
}

func unblob(b []byte) (int, []byte, error) {
	var version int
	i := bytes.IndexByte(b, '|')
	if i < 0 {
		return 0, nil, errors.New("bad blob")
	}
	if _, err := fmt.Sscanf(string(b[:i]), "v%d", &version); err != nil {
		return 0, nil, err
	}
	return version, b[i+1:], nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── AWS KMS mock ─────────────────────────────────────────────────────────────

const (
	awsTestKeyID  = "arn:aws:kms:eu-west-1:111122223333:key/test"
	awsTestAccess = "AKIDTEST"
	awsTestSecret = "secret/test/key"
	awsTestToken  = "session-token"
)

type awsMock struct {
	ring         *keyRing
	rotationDays int
}

func (m *awsMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	// Re-sign the request as received and require an identical signature.
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"__type": "MissingDate"})
		return
	}
	check, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	for _, h := range []string{"Content-Type", "X-Amz-Target"} {
		check.Header.Set(h, r.Header.Get(h))
	}
	signAWSv4(check, body, awsTestAccess, awsTestSecret, r.Header.Get("X-Amz-Security-Token"), "eu-west-1", "kms", when)
	if check.Header.Get("Authorization") != r.Header.Get("Authorization") || r.Header.Get("X-Amz-Security-Token") != awsTestToken {
		writeJSON(w, 403, map[string]string{"__type": "InvalidSignatureException", "message": "signature mismatch"})
		return
	}

	var in struct {
		KeyId, DestinationKeyId   string
		Plaintext, CiphertextBlob []byte
		RotationPeriodInDays      int
	}
	_ = json.Unmarshal(body, &in)
	switch r.Header.Get("X-Amz-Target") {
	case "TrentService.Encrypt":
		v, sealed := m.ring.encrypt(in.Plaintext)
		writeJSON(w, 200, map[string]interface{}{"CiphertextBlob": blob(v, sealed), "KeyId": in.KeyId})
	case "TrentService.Decrypt":
		v, sealed, err := unblob(in.CiphertextBlob)
		plain, derr := m.ring.decrypt(v, sealed)
		if err != nil || derr != nil {
			writeJSON(w, 400, map[string]string{"__type": "InvalidCiphertextException"})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"Plaintext": plain})
	case "TrentService.ReEncrypt":
		v, sealed, _ := unblob(in.CiphertextBlob)
		plain, err := m.ring.decrypt(v, sealed)
		if err != nil || in.DestinationKeyId != awsTestKeyID {
			writeJSON(w, 400, map[string]string{"__type": "InvalidCiphertextException"})
			return
		}
		nv, resealed := m.ring.encrypt(plain)
		writeJSON(w, 200, map[string]interface{}{"CiphertextBlob": blob(nv, resealed)})
	case "TrentService.EnableKeyRotation":
		m.rotationDays = in.RotationPeriodInDays
		w.WriteHeader(200)
	default:
		writeJSON(w, 400, map[string]string{"__type": "UnknownOperationException"})
	}
}

func newAWSTestKMS(t *testing.T) (*AWSKMS, *awsMock) {
	t.Helper()
	mock := &awsMock{ring: newKeyRing()}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	k, err := NewAWSKMS(AWSKMSConfig{Region: "eu-west-1", KeyID: awsTestKeyID, AccessKeyID: awsTestAccess,
		SecretAccessKey: awsTestSecret, SessionToken: awsTestToken, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return k, mock
}

// ── Google Cloud KMS mock ────────────────────────────────────────────────────

const gcpTestKey = "projects/p/locations/global/keyRings/r/cryptoKeys/k"

type gcpMock struct {
	ring          *keyRing
	token         string
	metadataCalls int
	rotation      map[string]string
	query         string
}

func (m *gcpMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/metadata" {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(403)
			return
		}
		m.metadataCalls++
		writeJSON(w, 200, map[string]interface{}{"access_token": m.token, "expires_in": 3600, "token_type": "Bearer"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+m.token {
		writeJSON(w, 401, map[string]interface{}{"error": map[string]string{"status": "UNAUTHENTICATED", "message": "bad token"}})
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/"+gcpTestKey+":encrypt":
		var in struct{ Plaintext []byte }
		_ = json.Unmarshal(body, &in)
		v, sealed := m.ring.encrypt(in.Plaintext)
		writeJSON(w, 200, map[string]interface{}{"name": fmt.Sprintf("%s/cryptoKeyVersions/%d", gcpTestKey, v), "ciphertext": blob(v, sealed)})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/"+gcpTestKey+":decrypt":
		var in struct{ Ciphertext []byte }
		_ = json.Unmarshal(body, &in)
		v, sealed, err := unblob(in.Ciphertext)
		plain, derr := m.ring.decrypt(v, sealed)
		if err != nil || derr != nil {
			writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"status": "INVALID_ARGUMENT", "message": "decryption failed"}})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"plaintext": plain})
	case r.Method == http.MethodPatch && r.URL.Path == "/v1/"+gcpTestKey:
		m.query = r.URL.RawQuery
		_ = json.Unmarshal(body, &m.rotation)
		writeJSON(w, 200, map[string]string{"name": gcpTestKey})
	default:
		writeJSON(w, 404, map[string]interface{}{"error": map[string]string{"status": "NOT_FOUND"}})
	}
}

func newGCPTestKMS(t *testing.T) (*GCPKMS, *gcpMock) {
	t.Helper()
	mock := &gcpMock{ring: newKeyRing(), token: "ya29.test"}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	k, err := NewGCPKMS(GCPKMSConfig{KeyName: gcpTestKey, Endpoint: srv.URL, MetadataURL: srv.URL + "/metadata"})
	if err != nil {
		t.Fatal(err)
	}
	return k, mock
}

// ── Vault Transit mock ───────────────────────────────────────────────────────

type vaultMock struct {
	ring         *keyRing
	rotateConfig map[string]string
}

func (m *vaultMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Vault-Token") != "s.test" || r.Header.Get("X-Vault-Namespace") != "nexora" {
		writeJSON(w, 403, map[string][]string{"errors": {"permission denied"}})
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in struct {
		Plaintext  []byte `json:"plaintext"`
		Ciphertext string `json:"ciphertext"`
	}
	_ = json.Unmarshal(body, &in)
	vaultCT := func(v int, sealed []byte) string {
		return fmt.Sprintf("vault:v%d:%s", v, base64.StdEncoding.EncodeToString(sealed))
	}
	parse := func(ct string) (int, []byte, error) {
		parts := strings.SplitN(ct, ":", 3)
		if len(parts) != 3 || parts[0] != "vault" {
			return 0, nil, errors.New("bad ciphertext")
		}
		var v int
		if _, err := fmt.Sscanf(parts[1], "v%d", &v); err != nil {
			return 0, nil, err
		}
		sealed, err := base64.StdEncoding.DecodeString(parts[2])
		return v, sealed, err
	}
	switch r.URL.Path {
	case "/v1/transit/encrypt/seeds":
		v, sealed := m.ring.encrypt(in.Plaintext)
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"ciphertext": vaultCT(v, sealed), "key_version": v}})
	case "/v1/transit/decrypt/seeds":
		v, sealed, err := parse(in.Ciphertext)
		plain, derr := m.ring.decrypt(v, sealed)
		if err != nil || derr != nil {
			writeJSON(w, 400, map[string][]string{"errors": {"cipher: message authentication failed"}})
			return
		}
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"plaintext": plain}})
	case "/v1/transit/rewrap/seeds":
		v, sealed, _ := parse(in.Ciphertext)
		plain, err := m.ring.decrypt(v, sealed)
		if err != nil {
			writeJSON(w, 400, map[string][]string{"errors": {"invalid ciphertext"}})
			return
		}
		nv, resealed := m.ring.encrypt(plain)
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"ciphertext": vaultCT(nv, resealed)}})
	case "/v1/transit/keys/seeds/config":
		_ = json.Unmarshal(body, &m.rotateConfig)
		w.WriteHeader(204)
	default:
		writeJSON(w, 404, map[string][]string{"errors": {"no handler"}})
	}
}

func newVaultTestKMS(t *testing.T) (*VaultTransit, *vaultMock) {
	t.Helper()
	mock := &vaultMock{ring: newKeyRing()}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	k, err := NewVaultTransit(VaultConfig{Address: srv.URL, Token: "s.test", Namespace: "nexora", Key: "seeds"})
	if err != nil {
		t.Fatal(err)
	}
	return k, mock
}

// ── tests ────────────────────────────────────────────────────────────────────

type providerCase struct {
	name string
	kms  KMS
	ring *keyRing
}

func providers(t *testing.T) []providerCase {
	aws, awsM := newAWSTestKMS(t)
	gcp, gcpM := newGCPTestKMS(t)
	vault, vaultM := newVaultTestKMS(t)
	return []providerCase{{"aws-kms", aws, awsM.ring}, {"gcp-kms", gcp, gcpM.ring}, {"vault-transit", vault, vaultM.ring}}
}

func installKMS(t *testing.T, k KMS) {
	t.Helper()
	UseKMS(k)
	t.Cleanup(func() { UseKMS(nil) })
}

var (
	testSeed      = []byte("SBGWSG6BTNCKCOB3DIFBGCVMUPQFYPA2G4O34RMTB343OYPXU5DJDVMN")
	testLegacyKey = bytes.Repeat([]byte{7}, 32)
)

func TestEnvelope_SealOpenRoundTripPerProvider(t *testing.T) {
	for _, p := range providers(t) {
		t.Run(p.name, func(t *testing.T) {
			installKMS(t, p.kms)
			ctx := context.Background()

			sealed, err := SealSecret(ctx, testSeed, testLegacyKey)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(sealed), "v2::"+p.name+"::") || bytes.Contains(sealed, testSeed) {
				t.Fatalf("not a %s v2 envelope, or plaintext leaked: %.40s", p.name, sealed)
			}
			// Every secret gets its own data key.
			again, _ := SealSecret(ctx, testSeed, testLegacyKey)
			if bytes.Equal(sealed, again) || strings.Split(string(sealed), "::")[2] == strings.Split(string(again), "::")[2] {
				t.Fatal("two seals shared a wrapped data key")
			}

			var seen []byte
			err = OpenSecret(ctx, sealed, testLegacyKey, func(plaintext []byte) error {
				if !bytes.Equal(plaintext, testSeed) {
					t.Fatalf("opened %q", plaintext)
				}
				seen = plaintext
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(seen, make([]byte, len(testSeed))) {
				t.Fatal("plaintext buffer was not zeroized after use")
			}

			// The generic Decrypt path (used outside signing) also opens v2.
			plain, err := Decrypt(sealed, nil)
			if err != nil || !bytes.Equal(plain, testSeed) {
				t.Fatalf("Decrypt(v2) = %q, %v", plain, err)
			}
		})
	}
}

func TestEnvelope_RotationKeepsOldEnvelopesAndRewraps(t *testing.T) {
	for _, p := range providers(t) {
		t.Run(p.name, func(t *testing.T) {
			installKMS(t, p.kms)
			ctx := context.Background()
			sealed, err := SealSecret(ctx, testSeed, testLegacyKey)
			if err != nil {
				t.Fatal(err)
			}

			// The provider rotates the KEK on its schedule.
			p.ring.rotate()

			if err := OpenSecret(ctx, sealed, nil, func([]byte) error { return nil }); err != nil {
				t.Fatalf("envelope sealed before rotation no longer opens: %v", err)
			}
			if p.ring.lastDecryptVersion != 1 {
				t.Fatalf("decrypted with KEK v%d, want v1", p.ring.lastDecryptVersion)
			}

			rewrapped, err := RewrapSecret(ctx, sealed, nil)
			if err != nil {
				t.Fatal(err)
			}
			oldParts, newParts := strings.Split(string(sealed), "::"), strings.Split(string(rewrapped), "::")
			if oldParts[2] == newParts[2] || oldParts[3] != newParts[3] {
				t.Fatal("rewrap must replace the wrapped data key and leave the encrypted secret untouched")
			}
			err = OpenSecret(ctx, rewrapped, nil, func(plaintext []byte) error {
				if !bytes.Equal(plaintext, testSeed) {
					t.Fatalf("rewrapped envelope opened to %q", plaintext)
				}
				return nil
			})
			if err != nil || p.ring.lastDecryptVersion != 2 {
				t.Fatalf("rewrapped envelope: err=%v, KEK v%d (want v2)", err, p.ring.lastDecryptVersion)
			}
		})
	}
}

func TestEnsureRotation_AppliesProviderPolicy(t *testing.T) {
	ctx := context.Background()
	ninetyDays := RotationPolicy{Period: 90 * 24 * time.Hour}

	aws, awsM := newAWSTestKMS(t)
	if err := aws.EnsureRotation(ctx, ninetyDays); err != nil || awsM.rotationDays != 90 {
		t.Fatalf("aws: err=%v days=%d", err, awsM.rotationDays)
	}
	if err := aws.EnsureRotation(ctx, RotationPolicy{Period: 30 * 24 * time.Hour}); !errors.Is(err, ErrInvalidKMSConfig) {
		t.Fatalf("aws accepted a 30-day period: %v", err)
	}

	gcp, gcpM := newGCPTestKMS(t)
	gcp.now = func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) }
	if err := gcp.EnsureRotation(ctx, ninetyDays); err != nil {
		t.Fatal(err)
	}
	if gcpM.rotation["rotationPeriod"] != "7776000s" || gcpM.rotation["nextRotationTime"] != "2027-01-20T00:00:00Z" ||
		gcpM.query != "updateMask=rotationPeriod,nextRotationTime" {
		t.Fatalf("gcp rotation request: %v ?%s", gcpM.rotation, gcpM.query)
	}
	if err := gcp.EnsureRotation(ctx, RotationPolicy{Period: time.Hour}); !errors.Is(err, ErrInvalidKMSConfig) {
		t.Fatalf("gcp accepted 1h: %v", err)
	}

	vault, vaultM := newVaultTestKMS(t)
	if err := vault.EnsureRotation(ctx, ninetyDays); err != nil || vaultM.rotateConfig["auto_rotate_period"] != "7776000s" {
		t.Fatalf("vault: err=%v config=%v", err, vaultM.rotateConfig)
	}
	if err := vault.EnsureRotation(ctx, RotationPolicy{Period: time.Minute}); !errors.Is(err, ErrInvalidKMSConfig) {
		t.Fatalf("vault accepted 1m: %v", err)
	}
}

func TestEnvelope_RejectsTamperingAndMismatches(t *testing.T) {
	ps := providers(t)
	installKMS(t, ps[0].kms)
	ctx := context.Background()
	sealed, _ := SealSecret(ctx, testSeed, testLegacyKey)

	tampered := append([]byte(nil), sealed...)
	last := tampered[len(tampered)-1]
	if last == '0' {
		tampered[len(tampered)-1] = '1'
	} else {
		tampered[len(tampered)-1] = '0'
	}
	if err := OpenSecret(ctx, tampered, nil, func([]byte) error { return nil }); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("tampered ciphertext: want ErrDecryptionFailed, got %v", err)
	}

	// A payload cannot be replayed under another provider's header.
	relabelled := []byte(strings.Replace(string(sealed), "v2::aws-kms::", "v2::vault-transit::", 1))
	UseKMS(ps[2].kms)
	if err := OpenSecret(ctx, relabelled, nil, func([]byte) error { return nil }); err == nil {
		t.Fatal("relabelled envelope opened")
	}
	if err := OpenSecret(ctx, sealed, nil, func([]byte) error { return nil }); !errors.Is(err, ErrKMSProviderMismatch) {
		t.Fatalf("want ErrKMSProviderMismatch, got %v", err)
	}

	UseKMS(nil)
	if err := OpenSecret(ctx, sealed, nil, func([]byte) error { return nil }); !errors.Is(err, ErrKMSNotConfigured) {
		t.Fatalf("want ErrKMSNotConfigured, got %v", err)
	}
	if _, err := ParseEnvelope([]byte("v2::aws-kms::%%%::00")); err == nil {
		t.Fatal("v1 parser accepted a v2 envelope")
	}
}

func TestEnvelope_LegacyCompatibilityAndMigration(t *testing.T) {
	ctx := context.Background()
	UseKMS(nil)
	legacy, err := SealSecret(ctx, testSeed, testLegacyKey)
	if err != nil || !strings.HasPrefix(string(legacy), "v1::") {
		t.Fatalf("without a KMS SealSecret must keep producing v1: %.10s %v", legacy, err)
	}

	p := providers(t)[2]
	installKMS(t, p.kms)
	err = OpenSecret(ctx, legacy, testLegacyKey, func(plaintext []byte) error {
		if !bytes.Equal(plaintext, testSeed) {
			t.Fatalf("legacy opened to %q", plaintext)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("v1 secret must still open once a KMS is enabled: %v", err)
	}

	migrated, err := RewrapSecret(ctx, legacy, testLegacyKey)
	if err != nil || !strings.HasPrefix(string(migrated), "v2::vault-transit::") {
		t.Fatalf("migration: %.30s %v", migrated, err)
	}
	if err := OpenSecret(ctx, migrated, nil, func(plaintext []byte) error {
		if !bytes.Equal(plaintext, testSeed) {
			t.Fatalf("migrated opened to %q", plaintext)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSecret_ZeroizesEvenWhenCallbackFails(t *testing.T) {
	installKMS(t, providers(t)[1].kms)
	sealed, _ := SealSecret(context.Background(), testSeed, nil)
	var seen []byte
	boom := errors.New("signing failed")
	err := OpenSecret(context.Background(), sealed, nil, func(plaintext []byte) error {
		seen = plaintext
		return boom
	})
	if !errors.Is(err, boom) || !bytes.Equal(seen, make([]byte, len(testSeed))) {
		t.Fatalf("err=%v, buffer zeroized=%v", err, bytes.Equal(seen, make([]byte, len(testSeed))))
	}
}

func TestProviderErrorsAreSurfaced(t *testing.T) {
	vault, _ := newVaultTestKMS(t)
	vault.cfg.Token = "wrong"
	_, err := vault.Wrap(context.Background(), make([]byte, 32))
	if !errors.Is(err, ErrKMSRequest) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("vault 403: %v", err)
	}

	aws, _ := newAWSTestKMS(t)
	aws.cfg.SecretAccessKey = "wrong"
	_, err = aws.Wrap(context.Background(), make([]byte, 32))
	if !errors.Is(err, ErrKMSRequest) || !strings.Contains(err.Error(), "InvalidSignatureException") {
		t.Fatalf("aws bad signature: %v", err)
	}
}

func TestGCPMetadataTokenIsCached(t *testing.T) {
	gcp, mock := newGCPTestKMS(t)
	for i := 0; i < 3; i++ {
		if _, err := gcp.Wrap(context.Background(), make([]byte, 32)); err != nil {
			t.Fatal(err)
		}
	}
	if mock.metadataCalls != 1 {
		t.Fatalf("metadata server called %d times, want 1", mock.metadataCalls)
	}
	gcp.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := gcp.Wrap(context.Background(), make([]byte, 32)); err != nil || mock.metadataCalls != 2 {
		t.Fatalf("expired token not refreshed: calls=%d err=%v", mock.metadataCalls, err)
	}
}

// TestSigV4_AWSTestSuiteVector checks signAWSv4 against the "get-vanilla"
// case of AWS's published Signature Version 4 test suite.
func TestSigV4_AWSTestSuiteVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	signAWSv4(req, nil, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "", "us-east-1", "service",
		time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestNewKMS_ValidatesConfiguration(t *testing.T) {
	if k, err := NewKMS(KMSConfig{Provider: "none"}); k != nil || err != nil {
		t.Fatalf("none: %v %v", k, err)
	}
	for name, cfg := range map[string]KMSConfig{
		"unknown provider": {Provider: "hsm"},
		"aws without key":  {Provider: "aws", AWS: AWSKMSConfig{Region: "eu-west-1", AccessKeyID: "a", SecretAccessKey: "b"}},
		"gcp bad key name": {Provider: "gcp", GCP: GCPKMSConfig{KeyName: "k"}},
		"vault no token":   {Provider: "vault", Vault: VaultConfig{Address: "https://vault:8200", Key: "k"}},
	} {
		if _, err := NewKMS(cfg); !errors.Is(err, ErrInvalidKMSConfig) {
			t.Errorf("%s: want ErrInvalidKMSConfig, got %v", name, err)
		}
	}
}

func TestConfigureKMS_InstallsProviderAndAppliesRotation(t *testing.T) {
	_, mock := newVaultTestKMS(t)
	srv := httptest.NewServer(mock)
	defer srv.Close()
	t.Cleanup(func() { UseKMS(nil) })
	k, err := ConfigureKMS(context.Background(), KMSConfig{
		Provider: "vault", RotationPeriod: 24 * time.Hour,
		Vault: VaultConfig{Address: srv.URL, Token: "s.test", Namespace: "nexora", Key: "seeds"},
	})
	if err != nil || CurrentKMS() != k || mock.rotateConfig["auto_rotate_period"] != "86400s" {
		t.Fatalf("configure: err=%v installed=%v rotation=%v", err, CurrentKMS() == k, mock.rotateConfig)
	}
}

func TestZeroize(t *testing.T) {
	b := []byte("secret material")
	Zeroize(b)
	if !bytes.Equal(b, make([]byte, len(b))) {
		t.Fatalf("not zeroized: %q", b)
	}
}
