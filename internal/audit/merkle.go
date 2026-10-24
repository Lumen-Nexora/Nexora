package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
)

var (
	ErrTransactionProofNotFound = errors.New("transaction proof not found")
	ErrProofUnavailable         = errors.New("merkle proof unavailable")
)

// SettlementProof is the cryptographic witness auditors use to validate that a
// settled transaction was included in the daily merged settlement root.
type SettlementProof struct {
	TransactionID string   `json:"transaction_id"`
	Day           string   `json:"day"`
	Root          string   `json:"root"`
	Leaf          string   `json:"leaf"`
	Proof         []string `json:"proof"`
}

// SettlementLeaf canonicalizes a settled transaction into the leaf value used in
// the daily settlement Merkle tree.
func SettlementLeaf(tx *domain.Transaction) [32]byte {
	if tx == nil {
		return [32]byte{}
	}
	payload := strings.Join([]string{
		tx.ID,
		tx.TxHash,
		string(tx.Type),
		string(tx.Status),
		tx.Asset,
		tx.FromWallet,
		tx.ToWallet,
		tx.Reference,
		tx.CreatedAt.UTC().Format(time.RFC3339Nano),
	}, "|")
	return sha256.Sum256([]byte(payload))
}

// FilterSettledTransactions narrows a transaction set to exactly the UTC day and
// orders it deterministically so the same day yields the same Merkle tree.
func FilterSettledTransactions(transactions []*domain.Transaction, day time.Time) []*domain.Transaction {
	start := day.UTC().Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)

	filtered := make([]*domain.Transaction, 0, len(transactions))
	for _, tx := range transactions {
		if tx == nil || tx.Status != domain.StatusSettled {
			continue
		}
		created := tx.CreatedAt.UTC()
		if created.Before(start) || !created.Before(end) {
			continue
		}
		filtered = append(filtered, tx)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if !filtered[i].CreatedAt.Equal(filtered[j].CreatedAt) {
			return filtered[i].CreatedAt.Before(filtered[j].CreatedAt)
		}
		return filtered[i].ID < filtered[j].ID
	})
	return filtered
}

// ProofForSettledTransaction returns the Merkle proof for a settled transaction in a UTC day.
func ProofForSettledTransaction(day time.Time, transactions []*domain.Transaction, txID string) (*SettlementProof, error) {
	if txID == "" {
		return nil, fmt.Errorf("%w: tx_id is required", ErrTransactionProofNotFound)
	}

	filtered := FilterSettledTransactions(transactions, day)
	if len(filtered) == 0 {
		return nil, fmt.Errorf("%w: no settled transactions for %s", ErrTransactionProofNotFound, day.UTC().Format("2006-01-02"))
	}

	leaves := make([][32]byte, 0, len(filtered))
	index := -1
	for i, tx := range filtered {
		if tx == nil {
			continue
		}
		if tx.ID == txID {
			index = i
		}
		leaves = append(leaves, SettlementLeaf(tx))
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: %s", ErrTransactionProofNotFound, txID)
	}

	root, err := MerkleRoot(leaves)
	if err != nil {
		return nil, err
	}
	path, err := MerkleProof(leaves, index)
	if err != nil {
		return nil, err
	}

	proof := make([]string, 0, len(path))
	for _, part := range path {
		proof = append(proof, hex.EncodeToString(part[:]))
	}

	return &SettlementProof{
		TransactionID: txID,
		Day:           day.UTC().Format("2006-01-02"),
		Root:          hex.EncodeToString(root[:]),
		Leaf:          hex.EncodeToString(leaves[index][:]),
		Proof:         proof,
	}, nil
}

// ParseProof decodes a Merkle audit path into the hash values expected by the verifier.
func ParseProof(encoded []string) ([][32]byte, error) {
	proof := make([][32]byte, 0, len(encoded))
	for _, value := range encoded {
		decoded, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
		if err != nil {
			return nil, fmt.Errorf("decode proof element %q: %w", value, err)
		}
		if len(decoded) != 32 {
			return nil, fmt.Errorf("%w: proof element %q is %d bytes, want 32", ErrProofUnavailable, value, len(decoded))
		}
		var node [32]byte
		copy(node[:], decoded)
		proof = append(proof, node)
	}
	return proof, nil
}

// VerifySettlementProof checks a daily settlement inclusion proof by recomputing the path from the leaf.
func VerifySettlementProof(root [32]byte, tx *domain.Transaction, proof []string) bool {
	leaf := SettlementLeaf(tx)
	path, err := ParseProof(proof)
	if err != nil {
		return false
	}
	return VerifyProof(leaf, path, root)
}

type settlementProofSource interface {
	GetByID(ctx context.Context, id string) (*domain.Transaction, error)
	ListSettledBetween(ctx context.Context, from, to time.Time) ([]*domain.Transaction, error)
}

// GetProof reads a transaction by id and recomputes the proof for the UTC day in
// which it settled.
func GetProof(ctx context.Context, source settlementProofSource, txID string) (*SettlementProof, error) {
	if source == nil {
		return nil, ErrProofUnavailable
	}
	if txID == "" {
		return nil, fmt.Errorf("%w: tx_id is required", ErrTransactionProofNotFound)
	}

	tx, err := source.GetByID(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil || tx.Status != domain.StatusSettled {
		return nil, fmt.Errorf("%w: %s", ErrTransactionProofNotFound, txID)
	}

	day := tx.CreatedAt.UTC().Truncate(24 * time.Hour)
	transactions, err := source.ListSettledBetween(ctx, day, day.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	return ProofForSettledTransaction(day, transactions, txID)
}
