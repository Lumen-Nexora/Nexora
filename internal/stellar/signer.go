package stellar

import (
	"context"
	"crypto/ed25519"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/crypto"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/network"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

var (
	// ErrOfflineSignaturePending indicates that an unsigned transaction XDR has been
	// cached for asynchronous air-gapped signing and cannot be broadcast immediately.
	ErrOfflineSignaturePending = errors.New("offline signature pending: unsigned XDR cached for air-gapped signing")

	// ErrNoAuthorizedSigners indicates that no authorized signing public keys were configured.
	ErrNoAuthorizedSigners = errors.New("at least one authorized signing public key is required")

	// ErrUnauthorizedSignature indicates that an envelope carries a signature from a key
	// not in the authorized signers list, or fails cryptographic verification.
	ErrUnauthorizedSignature = errors.New("signature verification failed against authorized signing keys")

	// ErrInsufficientSignatures indicates that fewer valid signatures were provided than required.
	ErrInsufficientSignatures = errors.New("insufficient valid signatures for transaction broadcast")
)

// Signer signs a Stellar transaction. The abstraction boundary that separates
// key management from transaction building — replace EnvSigner with an HSM,
// KMS, or air-gapped OfflineSigner implementation without touching the settlement engine.
type Signer interface {
	Sign(tx *txnbuild.Transaction, encryptedSecret string) (*txnbuild.Transaction, error)
}

// EnvSigner decrypts the wallet secret from AES-256-GCM storage and signs in-process.
type EnvSigner struct {
	masterKey         []byte
	networkPassphrase string
}

func NewEnvSigner(masterKey []byte, stellarNetwork string) *EnvSigner {
	passphrase := ResolveNetworkPassphrase(stellarNetwork)
	return &EnvSigner{masterKey: masterKey, networkPassphrase: passphrase}
}

// ResolveNetworkPassphrase maps a network identifier ("mainnet", "public", "testnet", or a raw passphrase)
// to the canonical Stellar network passphrase.
func ResolveNetworkPassphrase(stellarNetwork string) string {
	switch strings.ToLower(strings.TrimSpace(stellarNetwork)) {
	case "mainnet", "public":
		return network.PublicNetworkPassphrase
	case "", "testnet":
		return network.TestNetworkPassphrase
	default:
		return stellarNetwork
	}
}

// Sign opens the wallet's sealed secret only for the moment it takes to sign
// (see SignWithSealedSecret). v2 envelopes are unwrapped through the
// configured KMS; v1 and legacy envelopes are decrypted with the master key.
func (s *EnvSigner) Sign(tx *txnbuild.Transaction, encryptedSecret string) (*txnbuild.Transaction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), signTimeout)
	defer cancel()
	return SignWithSealedSecret(ctx, tx, s.networkPassphrase, []byte(encryptedSecret), s.masterKey)
}

// signTimeout bounds the KMS round trip behind one signature.
const signTimeout = 15 * time.Second

// seedVersionByte is the strkey version byte of an ed25519 secret seed ("S...").
const seedVersionByte = 18 << 3

// SignWithSealedSecret signs tx with the Stellar secret seed sealed in
// envelope. The seed is decrypted into an ephemeral buffer, decoded into a
// raw ed25519 seed and private key, used for exactly one signature, and every
// one of those buffers is zeroized before this returns, so the seed never
// lingers as an immutable string (as keypair.Full would keep it).
func SignWithSealedSecret(ctx context.Context, tx *txnbuild.Transaction, networkPassphrase string, envelope, legacyKey []byte) (*txnbuild.Transaction, error) {
	hash, err := tx.Hash(networkPassphrase)
	if err != nil {
		return nil, fmt.Errorf("hash transaction: %w", err)
	}
	var sig xdr.DecoratedSignature
	err = crypto.OpenSecret(ctx, envelope, legacyKey, func(encodedSeed []byte) error {
		var signErr error
		sig, signErr = signHashWithSeed(hash[:], encodedSeed)
		return signErr
	})
	if err != nil {
		return nil, fmt.Errorf("sign with sealed secret: %w", err)
	}
	signed, err := tx.AddSignatureDecorated(sig)
	if err != nil {
		return nil, fmt.Errorf("attach signature: %w", err)
	}
	return signed, nil
}

// signHashWithSeed decodes a strkey secret seed held in a byte slice and signs
// hash with it, zeroizing the decoded seed and private key afterwards.
func signHashWithSeed(hash, encodedSeed []byte) (xdr.DecoratedSignature, error) {
	raw := make([]byte, base32.StdEncoding.DecodedLen(len(encodedSeed)))
	defer crypto.Zeroize(raw)
	n, err := base32.StdEncoding.Decode(raw, encodedSeed)
	if err != nil || n != 35 || raw[0] != seedVersionByte {
		return xdr.DecoratedSignature{}, errors.New("invalid secret seed")
	}
	if crc16XModem(raw[:33]) != binary.LittleEndian.Uint16(raw[33:35]) {
		return xdr.DecoratedSignature{}, errors.New("invalid secret seed checksum")
	}

	private := ed25519.NewKeyFromSeed(raw[1:33])
	defer crypto.Zeroize(private)
	var hint [4]byte
	copy(hint[:], private[ed25519.PrivateKeySize-4:]) // last 4 bytes of the public key
	return xdr.NewDecoratedSignature(ed25519.Sign(private, hash), hint), nil
}

// crc16XModem is the strkey checksum (CRC-16/XMODEM).
func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// VerifyTransactionSignatures cryptographically verifies every decorated signature on tx
// against the set of authorized Stellar public keys (G... addresses).
// It returns the list of unique authorized public keys that validly signed tx, or an error
// if any attached signature is invalid/unauthorized or if fewer than minRequired valid
// signatures are present.
func VerifyTransactionSignatures(tx *txnbuild.Transaction, networkPassphrase string, authorizedPublicKeys []string, minRequired int) ([]string, error) {
	if tx == nil {
		return nil, errors.New("transaction is nil")
	}
	if minRequired <= 0 {
		minRequired = 1
	}
	if len(authorizedPublicKeys) == 0 {
		return nil, ErrNoAuthorizedSigners
	}

	authorizedKPs := make([]*keypair.FromAddress, 0, len(authorizedPublicKeys))
	seenAuth := make(map[string]struct{}, len(authorizedPublicKeys))
	for _, rawKey := range authorizedPublicKeys {
		addr := strings.TrimSpace(rawKey)
		if addr == "" {
			continue
		}
		if _, dup := seenAuth[addr]; dup {
			continue
		}
		kp, err := keypair.ParseAddress(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid authorized public key %q: %w", addr, err)
		}
		seenAuth[addr] = struct{}{}
		authorizedKPs = append(authorizedKPs, kp)
	}
	if len(authorizedKPs) == 0 {
		return nil, ErrNoAuthorizedSigners
	}

	txHash, err := tx.Hash(networkPassphrase)
	if err != nil {
		return nil, fmt.Errorf("compute transaction hash for verification: %w", err)
	}

	sigs := tx.Signatures()
	if len(sigs) == 0 {
		return nil, fmt.Errorf("%w: transaction has 0 signatures (need %d)", ErrInsufficientSignatures, minRequired)
	}

	verifiedSigners := make([]string, 0, len(sigs))
	verifiedSet := make(map[string]struct{}, len(sigs))

	for _, decoratedSig := range sigs {
		matched := false
		for _, kp := range authorizedKPs {
			if kp.Hint() != decoratedSig.Hint {
				continue
			}
			if err := kp.Verify(txHash[:], decoratedSig.Signature); err == nil {
				matched = true
				if _, exists := verifiedSet[kp.Address()]; !exists {
					verifiedSet[kp.Address()] = struct{}{}
					verifiedSigners = append(verifiedSigners, kp.Address())
				}
				break
			}
		}
		if !matched {
			return nil, fmt.Errorf("%w: signature with hint %x does not match any authorized signing key", ErrUnauthorizedSignature, decoratedSig.Hint)
		}
	}

	if len(verifiedSigners) < minRequired {
		return nil, fmt.Errorf("%w: got %d valid authorized signatures, require at least %d", ErrInsufficientSignatures, len(verifiedSigners), minRequired)
	}

	return verifiedSigners, nil
}
