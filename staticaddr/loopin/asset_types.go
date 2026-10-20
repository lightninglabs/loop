package loopin

import (
	"context"
	"encoding/json"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit"
)

// AssetSwapRequest fixes user-approved native quantities before any server
// call.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type AssetSwapRequest struct {
	RequestID [32]byte `json:"RequestID"`
	GroupKey  [33]byte `json:"GroupKey"`
	Outpoint  string   `json:"Outpoint"`
	Amount    uint64   `json:"Amount"` // Exact native Lightning payout.
	MaxDebit  uint64   `json:"MaxDebit"`
	MaxFee    uint64   `json:"MaxFee"`
}

// validAssetSwapAmount spends the complete deposit without a change output. Its
// excess over the exact payout must fit the caller's native service fee cap.
func validAssetSwapAmount(depositAmount uint64, request AssetSwapRequest) bool {
	return request.Amount > 0 && request.Amount <= depositAmount &&
		request.MaxDebit >= request.Amount &&
		depositAmount-request.Amount <= request.MaxFee
}

// AssetJob persists creation identity before a possibly ambiguous remote
// effect.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type AssetJob struct {
	// Funding contains only the unfinished wallet transaction checkpoint.
	Funding              json.RawMessage               `json:"Funding,omitempty"`
	ID                   [32]byte                      `json:"ID"`
	Deposit              *swapkit.CreateDepositRequest `json:"Deposit"`
	Swap                 *AssetSwapRequest             `json:"Swap"`
	Refund               *swapkit.RefundDepositRequest `json:"Refund"`
	RefundProof          []byte                        `json:"RefundProof"`
	Offer                *assetexec.Offer              `json:"Offer"`
	Intent               []byte                        `json:"Intent"`
	ServerRequest        []byte                        `json:"ServerRequest"`
	Complete             bool                          `json:"Complete"`
	Canceled             bool                          `json:"Canceled"`
	RefundReported       bool                          `json:"RefundReported"`
	CancellationReported bool                          `json:"CancellationReported"`
}

// AssetInventoryEntry binds one isolated asset anchor to a single active
// request.
// Consumed anchors remain excluded from every Bitcoin-only spending path.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type AssetInventoryEntry struct {
	Deposit   swapkit.Deposit `json:"Deposit"`
	RequestID [32]byte        `json:"RequestID"`
	Hash      [32]byte        `json:"Hash"`
	Consumed  bool            `json:"Consumed"`
}

// AssetStore extends the normal loop-in store with asset deposit metadata,
// admission requests, and execution checkpoints in one transaction domain.
type AssetStore interface {
	assetexec.Store
	PutAssetJob(context.Context, *AssetJob) error
	UpdateAssetJob(context.Context, *AssetJob) error
	GetAssetJob(context.Context, [32]byte) (*AssetJob, error)
	AssetJobs(context.Context) ([]AssetJob, error)
	AssetInventory(context.Context) ([]AssetInventoryEntry, error)
	AddAssetDeposit(context.Context, swapkit.Deposit) error
	FinishAssetRefund(context.Context, *AssetJob, []byte) error
	CancelUnstartedAssetJob(context.Context, *AssetJob) error
}
