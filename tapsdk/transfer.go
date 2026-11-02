package tapsdk

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// FundedTransfer represents the result of funding a virtual transaction.
type FundedTransfer struct {
	// FundedPsbt is the funded virtual transaction PSBT.
	FundedPsbt []byte

	// PassiveAssetPsbts are the PSBTs for passive assets that need to be
	// re-signed.
	PassiveAssetPsbts [][]byte
}

// CommittedTransfer represents the result of committing virtual transactions.
type CommittedTransfer struct {
	// AnchorPsbt is the PSBT of the anchor transaction.
	AnchorPsbt []byte

	// VirtualPsbts are the committed virtual transaction PSBTs.
	VirtualPsbts [][]byte

	// PassiveAssetPsbts are the updated passive asset PSBTs.
	PassiveAssetPsbts [][]byte
}

// Recipient represents a recipient of an asset transfer.
type Recipient struct {
	// Address is the Taproot Asset address of the recipient.
	Address string

	amount    uint64
	hasAmount bool
}

// RecipientWithAmount creates a recipient with an explicit sender-chosen
// amount.
func RecipientWithAmount(address string, amount uint64) Recipient {
	return Recipient{
		Address:   address,
		amount:    amount,
		hasAmount: true,
	}
}

// RecipientWithEmbeddedAmount creates a recipient that uses the amount encoded
// in the Taproot Asset address.
func RecipientWithEmbeddedAmount(address string) Recipient {
	return Recipient{
		Address: address,
	}
}

// Amount returns the explicit amount and whether one was supplied. If the
// boolean is false, the recipient uses the amount embedded in the address.
func (r Recipient) Amount() (uint64, bool) {
	return r.amount, r.hasAmount
}

// InteractiveSendRequest represents a request to send assets interactively.
type InteractiveSendRequest struct {
	// AssetRef identifies the asset to send.
	AssetRef AssetRef

	// Amount is the number of asset units to send.
	Amount uint64

	// ReceiverKeys contains the keys derived by the receiver.
	ReceiverKeys DerivedKeys
}

// TransferOutput represents a single output in a transfer.
type TransferOutput struct {
	// AnchorOutpoint is the on-chain outpoint.
	AnchorOutpoint Outpoint

	// IssuanceID is the 32-byte protocol-level identifier of the
	// asset issuance/tranche created at this output.
	IssuanceID AssetID

	// AssetType is the type of the transferred asset.
	AssetType AssetType

	// AnchorValue is the BTC value of the anchor output in sats.
	AnchorValue int64

	// ScriptKey is the 33-byte compressed public key locking this output.
	ScriptKey PubKey

	// Amount is the number of asset units in this output.
	Amount uint64

	// ProofBlob is the proof suffix returned by tapd for this output.
	// A receiver needs a complete proof file, which can be exported after
	// the anchor transaction confirms by using IssuanceID, ScriptKey, and
	// AnchorOutpoint.
	ProofBlob []byte

	// AltLeaves are auxiliary Taproot leaves (if present) decoded from proofs.
	AltLeaves [][]byte

	// GroupKey is the asset's group public key when the asset belongs to
	// a group, or nil otherwise. For collectibles, the high-level AssetRef
	// remains the IssuanceID even when GroupKey is set.
	GroupKey *PubKey
}

// TransferInput represents a single input in a transfer.
type TransferInput struct {
	// AnchorPoint is the old/current location of the Taproot Asset commitment
	// that was spent as an input.
	AnchorPoint Outpoint

	// IssuanceID is the 32-byte protocol-level identifier of the asset
	// issuance/tranche that was spent.
	IssuanceID AssetID

	// AssetType is the type of the transferred asset.
	AssetType AssetType

	// ScriptKey is the 33-byte script key of the asset that was spent.
	ScriptKey PubKey

	// Amount is the number of asset units spent.
	Amount uint64

	// GroupKey is the asset's group public key when the asset belongs to
	// a group, or nil otherwise. For collectibles, the high-level AssetRef
	// remains the IssuanceID even when GroupKey is set.
	GroupKey *PubKey
}

// AssetTransfer represents a wallet-recorded outgoing transfer.
type AssetTransfer struct {
	// TransferTimestamp is the timestamp of the transfer in UTC Unix time
	// seconds.
	TransferTimestamp int64

	// TransferTxid is the anchor transaction ID (32 bytes, not reversed).
	TransferTxid [32]byte

	// AnchorTxid is the display-order transaction ID (string form).
	AnchorTxid string

	// AnchorTxHeightHint is the height hint of the anchor transaction.
	AnchorTxHeightHint uint32

	// AnchorTxChainFees is the total fees paid by the anchor transaction in
	// satoshis.
	AnchorTxChainFees int64

	// Inputs describes the set of spent assets.
	Inputs []TransferInput

	// Outputs describes the set of newly created asset outputs.
	Outputs []TransferOutput

	// AnchorTxBlockHash is the block hash of the blockchain block that contains
	// the anchor transaction (if confirmed).
	AnchorTxBlockHash [32]byte

	// AnchorTxBlockHashStr is the byte-reversed hash as a hex string (if
	// confirmed).
	AnchorTxBlockHashStr string

	// AnchorTxBlockHeight is the block height of the blockchain block that
	// contains the anchor transaction (0 if unconfirmed).
	AnchorTxBlockHeight uint32

	// Label is an optional short label for the transfer.
	Label string

	// AnchorTx is the raw anchor transaction bytes.
	AnchorTx []byte
}

// Transfer is a high-level wallet transfer keyed by AssetRef.
type Transfer struct {
	// TransferTimestamp is the timestamp of the transfer in UTC Unix time
	// seconds.
	TransferTimestamp int64

	// AnchorTxid is the display-order transaction ID of the anchor
	// transaction.
	AnchorTxid string

	// AnchorTxBlockHeight is the block height containing the anchor
	// transaction, or zero if the transfer is unconfirmed.
	AnchorTxBlockHeight uint32

	// AnchorTxChainFees is the total fee paid by the anchor transaction in
	// satoshis.
	AnchorTxChainFees int64

	// Label is the optional transfer label.
	Label string

	// Inputs are the assets spent by this transfer.
	Inputs []TransferAsset

	// Outputs are the assets created by this transfer.
	Outputs []TransferAsset
}

// TransferAsset describes one asset amount in a high-level transfer.
type TransferAsset struct {
	// AssetRef is the SDK identifier for the logical asset.
	AssetRef AssetRef

	// IssuanceID is the concrete protocol issuance/tranche ID.
	IssuanceID AssetID

	// Type is the transferred asset type.
	Type AssetType

	// Amount is the number of asset units.
	Amount uint64

	// ScriptKey is the script key for this transfer item.
	ScriptKey PubKey

	// Outpoint is the anchor outpoint for this transfer item.
	Outpoint Outpoint
}

// NewTransfer projects a raw AssetTransfer into the high-level, AssetRef-keyed
// Transfer. The AssetRef on each input and output is built from AssetType,
// GroupKey, and IssuanceID: fungibles prefer group-key refs, while
// collectibles use their concrete asset-ID refs.
//
// Returns nil when raw is nil.
func NewTransfer(raw *AssetTransfer) *Transfer {
	if raw == nil {
		return nil
	}

	transfer := &Transfer{
		TransferTimestamp:   raw.TransferTimestamp,
		AnchorTxid:          raw.AnchorTxid,
		AnchorTxBlockHeight: raw.AnchorTxBlockHeight,
		AnchorTxChainFees:   raw.AnchorTxChainFees,
		Label:               raw.Label,
		Inputs: make(
			[]TransferAsset, 0, len(raw.Inputs),
		),
		Outputs: make(
			[]TransferAsset, 0, len(raw.Outputs),
		),
	}

	for _, input := range raw.Inputs {
		transfer.Inputs = append(transfer.Inputs, TransferAsset{
			AssetRef: AssetRefFromTypedAsset(
				input.IssuanceID, input.GroupKey,
				input.AssetType,
			),
			IssuanceID: input.IssuanceID,
			Type:       input.AssetType,
			Amount:     input.Amount,
			ScriptKey:  input.ScriptKey,
			Outpoint:   input.AnchorPoint,
		})
	}

	for _, output := range raw.Outputs {
		transfer.Outputs = append(transfer.Outputs, TransferAsset{
			AssetRef: AssetRefFromTypedAsset(
				output.IssuanceID, output.GroupKey,
				output.AssetType,
			),
			IssuanceID: output.IssuanceID,
			Type:       output.AssetType,
			Amount:     output.Amount,
			ScriptKey:  output.ScriptKey,
			Outpoint:   output.AnchorOutpoint,
		})
	}

	return transfer
}

// Outpoint represents a Bitcoin transaction outpoint.
type Outpoint struct {
	// Txid is the 32-byte transaction hash.
	Txid [32]byte

	// Index is the output index within the transaction.
	Index uint32
}

// String returns the outpoint in "txid:index" format.
func (o Outpoint) String() string {
	hash, _ := chainhash.NewHash(o.Txid[:])
	return fmt.Sprintf("%v:%d", hash, o.Index)
}

// NewOutpointFromStr parses an outpoint from a string in "txid:index" format.
func NewOutpointFromStr(s string) (Outpoint, error) {
	op, err := wire.NewOutPointFromString(s)
	if err != nil {
		return Outpoint{}, err
	}

	return Outpoint{
		Txid:  op.Hash,
		Index: op.Index,
	}, nil
}
