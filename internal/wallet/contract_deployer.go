package wallet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/Lumen-Nexora/Nexora/internal/stellar"
	"github.com/stellar/go/txnbuild"
	"github.com/stellar/go/xdr"
)

// sorobanDeployer creates a contract instance from an already-uploaded WASM
// hash, passing the wallet policy as constructor arguments so the contract is
// initialized in the same transaction that creates it. A separate initialize
// call would leave a window for someone else to claim ownership first.
type sorobanDeployer struct {
	soroban  stellar.SorobanClient
	signer   stellar.Signer
	wasmHash string
}

// NewSorobanDeployer builds a deployer for the contract wallet WASM identified
// by wasmHash, which must already be installed on the network.
func NewSorobanDeployer(soroban stellar.SorobanClient, signer stellar.Signer, wasmHash string) ContractDeployer {
	return &sorobanDeployer{soroban: soroban, signer: signer, wasmHash: wasmHash}
}

func (d *sorobanDeployer) Deploy(ctx context.Context, owner string, params ContractWalletParams) (string, error) {
	if d.wasmHash == "" {
		return "", domain.ErrContractWasmNotConfigured
	}

	wasmHash, err := decodeWasmHash(d.wasmHash)
	if err != nil {
		return "", err
	}

	ownerAccount, err := xdr.AddressToAccountId(owner)
	if err != nil {
		return "", fmt.Errorf("decode owner address %s: %w", owner, err)
	}

	var salt xdr.Uint256
	if _, err := rand.Read(salt[:]); err != nil {
		return "", fmt.Errorf("generate contract salt: %w", err)
	}

	constructorArgs, err := buildConstructorArgs(owner, params)
	if err != nil {
		return "", err
	}

	op := &txnbuild.InvokeHostFunction{
		HostFunction: xdr.HostFunction{
			Type: xdr.HostFunctionTypeHostFunctionTypeCreateContractV2,
			CreateContractV2: &xdr.CreateContractArgsV2{
				ContractIdPreimage: xdr.ContractIdPreimage{
					Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
					FromAddress: &xdr.ContractIdPreimageFromAddress{
						Address: xdr.ScAddress{
							Type:      xdr.ScAddressTypeScAddressTypeAccount,
							AccountId: &ownerAccount,
						},
						Salt: salt,
					},
				},
				Executable: xdr.ContractExecutable{
					Type:     xdr.ContractExecutableTypeContractExecutableWasm,
					WasmHash: &wasmHash,
				},
				ConstructorArgs: constructorArgs,
			},
		},
		SourceAccount: owner,
	}

	tx, err := d.soroban.PrepareInvocation(ctx, owner, op)
	if err != nil {
		return "", fmt.Errorf("prepare contract deployment: %w", err)
	}

	if d.signer == nil {
		return "", domain.ErrNoContractSigner
	}
	signed, err := d.signer.Sign(tx, "")
	if err != nil {
		return "", fmt.Errorf("sign contract deployment: %w", err)
	}

	if _, err := d.soroban.SubmitTransaction(ctx, signed); err != nil {
		return "", fmt.Errorf("submit contract deployment: %w", err)
	}

	return contractIDFromPreimage(ownerAccount, salt, d.soroban.NetworkPassphrase())
}

// buildConstructorArgs mirrors the __constructor signature in the contract.
func buildConstructorArgs(owner string, params ContractWalletParams) (xdr.ScVec, error) {
	ownerArg, err := stellar.AccountScVal(owner)
	if err != nil {
		return nil, err
	}

	guardians := make(xdr.ScVec, 0, len(params.Guardians))
	for _, g := range params.Guardians {
		arg, err := stellar.AddressScVal(g)
		if err != nil {
			return nil, fmt.Errorf("encode guardian %s: %w", g, err)
		}
		guardians = append(guardians, arg)
	}

	threshold := xdr.Uint32(params.RecoveryThreshold)
	guardiansVec := &guardians

	return xdr.ScVec{
		ownerArg,
		{Type: xdr.ScValTypeScvVec, Vec: &guardiansVec},
		{Type: xdr.ScValTypeScvU32, U32: &threshold},
		stellar.I128ScVal(toStroops(params.SpendingLimit)),
		stellar.U64ScVal(params.SpendingWindowSeconds),
	}, nil
}

func decodeWasmHash(hexHash string) (xdr.Hash, error) {
	var hash xdr.Hash
	decoded, err := hex.DecodeString(hexHash)
	if err != nil {
		return hash, fmt.Errorf("decode contract wasm hash: %w", err)
	}
	if len(decoded) != len(hash) {
		return hash, fmt.Errorf("contract wasm hash must be %d bytes, got %d", len(hash), len(decoded))
	}
	copy(hash[:], decoded)
	return hash, nil
}

// contractIDFromPreimage derives the deterministic contract address that the
// network assigns to a from-address deployment, so the caller does not have to
// wait for the transaction to be included to learn it.
func contractIDFromPreimage(deployer xdr.AccountId, salt xdr.Uint256, passphrase string) (string, error) {
	preimage := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeContractId,
		ContractId: &xdr.HashIdPreimageContractId{
			NetworkId: xdr.Hash(sha256.Sum256([]byte(passphrase))),
			ContractIdPreimage: xdr.ContractIdPreimage{
				Type: xdr.ContractIdPreimageTypeContractIdPreimageFromAddress,
				FromAddress: &xdr.ContractIdPreimageFromAddress{
					Address: xdr.ScAddress{
						Type:      xdr.ScAddressTypeScAddressTypeAccount,
						AccountId: &deployer,
					},
					Salt: salt,
				},
			},
		},
	}

	encoded, err := preimage.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("encode contract id preimage: %w", err)
	}

	return stellar.EncodeContractID(sha256.Sum256(encoded))
}
