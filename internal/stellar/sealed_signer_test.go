package stellar

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/txnbuild"
)

// localKMS is an in-process KEK standing in for AWS KMS / Cloud KMS / Vault.
type localKMS struct {
	key     []byte
	unwraps int
}

func newLocalKMS() *localKMS {
	k := &localKMS{key: make([]byte, 32)}
	_, _ = rand.Read(k.key)
	return k
}

func (k *localKMS) gcm() cipher.AEAD {
	block, _ := aes.NewCipher(k.key)
	g, _ := cipher.NewGCM(block)
	return g
}
func (k *localKMS) Name() string { return "local-kms" }
func (k *localKMS) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	g := k.gcm()
	nonce := make([]byte, g.NonceSize())
	_, _ = rand.Read(nonce)
	return append(nonce, g.Seal(nil, nonce, dek, nil)...), nil
}
func (k *localKMS) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	k.unwraps++
	g := k.gcm()
	return g.Open(nil, wrapped[:g.NonceSize()], wrapped[g.NonceSize():], nil)
}
func (k *localKMS) Rewrap(ctx context.Context, wrapped []byte) ([]byte, error) {
	dek, err := k.Unwrap(ctx, wrapped)
	if err != nil {
		return nil, err
	}
	return k.Wrap(ctx, dek)
}
func (k *localKMS) EnsureRotation(context.Context, crypto.RotationPolicy) error { return nil }

func testTx(t *testing.T, source string) *txnbuild.Transaction {
	t.Helper()
	acct := horizon.Account{AccountID: source, Sequence: 1}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &acct, IncrementSequenceNum: true, BaseFee: txnbuild.MinBaseFee,
		Operations:    []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 9}},
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestEnvSigner_SignsKMSSealedSeedOnlyAtSigningTime(t *testing.T) {
	kms := newLocalKMS()
	crypto.UseKMS(kms)
	defer crypto.UseKMS(nil)

	masterKey := bytes.Repeat([]byte{3}, 32)
	kp := keypair.MustRandom()
	sealed, err := crypto.SealSecret(context.Background(), []byte(kp.Seed()), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte("v2::local-kms::")) {
		t.Fatalf("expected a v2 envelope, got %.20s", sealed)
	}

	signer := NewEnvSigner(masterKey, "testnet")
	if kms.unwraps != 0 {
		t.Fatal("data key unwrapped before signing")
	}
	signed, err := signer.Sign(testTx(t, kp.Address()), string(sealed))
	if err != nil {
		t.Fatal(err)
	}
	if kms.unwraps != 1 {
		t.Fatalf("want exactly one unwrap per signature, got %d", kms.unwraps)
	}
	if _, err := VerifyTransactionSignatures(signed, network.TestNetworkPassphrase, []string{kp.Address()}, 1); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	// Identical to what the SDK keypair would produce (ed25519 is deterministic).
	reference, _ := testTx(t, kp.Address()).Sign(network.TestNetworkPassphrase, kp)
	if !bytes.Equal(signed.Signatures()[0].Signature, reference.Signatures()[0].Signature) ||
		signed.Signatures()[0].Hint != reference.Signatures()[0].Hint {
		t.Fatal("signature differs from keypair.Full's")
	}
}

func TestEnvSigner_StillSignsMasterKeySeeds(t *testing.T) {
	crypto.UseKMS(nil)
	masterKey := bytes.Repeat([]byte{5}, 32)
	kp := keypair.MustRandom()
	sealed, err := crypto.Encrypt([]byte(kp.Seed()), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := NewEnvSigner(masterKey, "testnet").Sign(testTx(t, kp.Address()), string(sealed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTransactionSignatures(signed, network.TestNetworkPassphrase, []string{kp.Address()}, 1); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestSignHashWithSeed_RejectsCorruptSeeds(t *testing.T) {
	seed := []byte(keypair.MustRandom().Seed())
	hash := make([]byte, 32)

	corrupt := append([]byte(nil), seed...)
	if corrupt[10] == 'A' {
		corrupt[10] = 'B'
	} else {
		corrupt[10] = 'A'
	}
	if _, err := signHashWithSeed(hash, corrupt); err == nil {
		t.Fatal("seed with a bad checksum was accepted")
	}
	address := []byte(keypair.MustRandom().Address())
	if _, err := signHashWithSeed(hash, address); err == nil {
		t.Fatal("a public address was accepted as a seed")
	}
	if _, err := signHashWithSeed(hash, []byte("not base32!")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestSignWithSealedSecret_ReportsKMSFailure(t *testing.T) {
	kms := newLocalKMS()
	crypto.UseKMS(kms)
	defer crypto.UseKMS(nil)
	kp := keypair.MustRandom()
	sealed, _ := crypto.SealSecret(context.Background(), []byte(kp.Seed()), nil)

	crypto.UseKMS(nil) // KMS unavailable at signing time
	_, err := SignWithSealedSecret(context.Background(), testTx(t, kp.Address()), network.TestNetworkPassphrase, sealed, nil)
	if !errors.Is(err, crypto.ErrKMSNotConfigured) {
		t.Fatalf("want ErrKMSNotConfigured, got %v", err)
	}
}
