package tapsdk

import (
	"context"
	"sync"

	"github.com/lightninglabs/loop/tapsdk/internal/anchor"
)

// AnchorSigner signs and finalizes the BTC anchor PSBT returned by tapd after
// Commit. The callback should return the PSBT that PublishAndLogTransfer can
// extract and broadcast.
type AnchorSigner func(ctx context.Context, anchorPsbt []byte) ([]byte, error)

type txBuilderOptions struct {
	skipBroadcast bool
}

// TxBuilderOption configures TxBuilder finish/execute behavior.
type TxBuilderOption func(*txBuilderOptions)

// WithSkipBroadcast leaves the finalized anchor transaction unbroadcast.
func WithSkipBroadcast() TxBuilderOption {
	return func(o *txBuilderOptions) {
		o.skipBroadcast = true
	}
}

func applyTxBuilderOptions(opts []TxBuilderOption) *txBuilderOptions {
	o := &txBuilderOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

// TxBuilder builds address-based Taproot Asset transfers.
//
// Address-based transfers use Taproot Asset addresses, which include
// proof courier information for automatic proof delivery.
//
// For interactive transfers where the receiver provides keys directly,
// use InteractiveTxBuilder instead.
type TxBuilder struct {
	walletKit WalletKitClient

	recipients []Recipient
	inputs     []PrevID
	feeRate    FeeRate

	fundedPsbt   []byte
	passivePsbts [][]byte
	signedPsbt   []byte
	anchorPsbt   []byte

	changeOutputIndex int32
	lockedUTXOs       []Outpoint

	anchorSigner AnchorSigner
	finished     bool
	mu           sync.Mutex
}

// newTxBuilder creates a new TxBuilder instance.
func newTxBuilder(wallet WalletKitClient) *TxBuilder {
	feeRate, _ := NewFeeRateSatPerVByte(1)

	return &TxBuilder{
		walletKit:         wallet,
		feeRate:           feeRate,
		changeOutputIndex: -1,
	}
}

// AddRecipient adds a recipient with an explicit send amount.
func (b *TxBuilder) AddRecipient(address string, amount uint64) *TxBuilder {
	b.recipients = append(
		b.recipients, RecipientWithAmount(address, amount),
	)
	return b
}

// AddTapAddress adds a recipient that uses the amount embedded in the address.
func (b *TxBuilder) AddTapAddress(address string) *TxBuilder {
	b.recipients = append(
		b.recipients, RecipientWithEmbeddedAmount(address),
	)
	return b
}

// SetRecipients sets the recipients for the transaction.
func (b *TxBuilder) SetRecipients(recipients []Recipient) *TxBuilder {
	b.recipients = recipients
	return b
}

// AddInput adds a specific input to the transaction.
func (b *TxBuilder) AddInput(input PrevID) *TxBuilder {
	b.inputs = append(b.inputs, input)
	return b
}

// SetFeeRate sets the fee rate. Default is 1 sat/vB.
func (b *TxBuilder) SetFeeRate(feeRate FeeRate) *TxBuilder {
	b.feeRate = feeRate
	return b
}

// SetFundedPsbt injects an externally funded PSBT into the builder.
func (b *TxBuilder) SetFundedPsbt(fundedPsbt []byte) *TxBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.fundedPsbt = append([]byte(nil), fundedPsbt...)
	return b
}

// SetSignedPsbt injects an externally signed PSBT into the builder.
func (b *TxBuilder) SetSignedPsbt(signedPsbt []byte) *TxBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.signedPsbt = append([]byte(nil), signedPsbt...)
	return b
}

// SetAnchorPsbt injects an externally finalized anchor PSBT into the builder.
func (b *TxBuilder) SetAnchorPsbt(anchorPsbt []byte) *TxBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.anchorPsbt = append([]byte(nil), anchorPsbt...)
	return b
}

// SetPassivePsbts injects externally produced passive asset PSBTs.
func (b *TxBuilder) SetPassivePsbts(passivePsbts [][]byte) *TxBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.passivePsbts = clone2Dimensional(passivePsbts)
	return b
}

// SetAnchorSigner sets the callback used to finalize the BTC anchor PSBT
// between Commit and Finish.
func (b *TxBuilder) SetAnchorSigner(signer AnchorSigner) *TxBuilder {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.anchorSigner = signer
	return b
}

// Fund funds the transaction and returns the funded transfer details.
func (b *TxBuilder) Fund(ctx context.Context) (*FundedTransfer,
	error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}

	if len(b.recipients) == 0 {
		return nil, ErrNoRecipients
	}

	recipients, err := normaliseSendRecipients(b.recipients, true)
	if err != nil {
		return nil, wrapErr("Fund", err)
	}

	resp, err := b.walletKit.FundTransfer(ctx, recipients, b.inputs)
	if err != nil {
		return nil, wrapErr("Fund", err)
	}

	b.fundedPsbt = append([]byte(nil), resp.FundedPsbt...)
	b.passivePsbts = clone2Dimensional(resp.PassiveAssetPsbts)

	return resp, nil
}

// Sign signs the funded transaction and returns the signed PSBT.
func (b *TxBuilder) Sign(ctx context.Context) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}
	if len(b.fundedPsbt) == 0 {
		return nil, ErrNotFunded
	}

	signedPsbt, err := b.walletKit.SignVirtualPsbt(ctx, b.fundedPsbt)
	if err != nil {
		return nil, wrapErr("Sign", err)
	}

	b.signedPsbt = append([]byte(nil), signedPsbt...)
	return b.signedPsbt, nil
}

// Commit commits the signed transaction and returns the committed transfer.
func (b *TxBuilder) Commit(ctx context.Context) (
	*CommittedTransfer, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}
	if len(b.signedPsbt) == 0 {
		return nil, ErrNotSigned
	}

	resp, err := b.commitVirtualPsbts(ctx)
	if err != nil {
		return nil, wrapErr("Commit", err)
	}

	b.applyCommitResponse(resp)

	return committedTransferFromResponse(resp), nil
}

// Finish publishes the transaction and returns the finalized packet.
func (b *TxBuilder) Finish(ctx context.Context, opts ...TxBuilderOption) (
	*AssetPacket, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}
	if len(b.anchorPsbt) == 0 {
		return nil, ErrNotCommitted
	}

	if err := b.signAnchor(ctx); err != nil {
		return nil, wrapErr("Finish", err)
	}

	o := applyTxBuilderOptions(opts)
	resp, err := b.publishAndLog(ctx, o.skipBroadcast)
	if err != nil {
		return nil, wrapErr("Finish", err)
	}

	b.finished = true

	return resp, nil
}

// Execute funds, signs, commits, and publishes the transaction.
func (b *TxBuilder) Execute(ctx context.Context, opts ...TxBuilderOption) (
	*AssetPacket, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}

	// Fund if not already funded.
	if len(b.fundedPsbt) == 0 {
		if len(b.recipients) == 0 {
			return nil, ErrNoRecipients
		}

		recipients, err := normaliseSendRecipients(b.recipients, true)
		if err != nil {
			return nil, wrapErr("Fund", err)
		}

		resp, err := b.walletKit.FundTransfer(
			ctx, recipients, b.inputs,
		)
		if err != nil {
			return nil, wrapErr("Fund", err)
		}

		b.fundedPsbt = append([]byte(nil), resp.FundedPsbt...)
		b.passivePsbts = clone2Dimensional(resp.PassiveAssetPsbts)
	}

	// Sign.
	signedPsbt, err := b.walletKit.SignVirtualPsbt(ctx, b.fundedPsbt)
	if err != nil {
		return nil, wrapErr("Sign", err)
	}
	b.signedPsbt = append([]byte(nil), signedPsbt...)

	// Commit.
	commitResp, err := b.commitVirtualPsbts(ctx)
	if err != nil {
		return nil, wrapErr("Commit", err)
	}

	b.applyCommitResponse(commitResp)

	if err := b.signAnchor(ctx); err != nil {
		return nil, wrapErr("Finish", err)
	}

	// Finish.
	o := applyTxBuilderOptions(opts)
	resp, err := b.publishAndLog(ctx, o.skipBroadcast)
	if err != nil {
		return nil, wrapErr("Finish", err)
	}

	b.finished = true

	return resp, nil
}

func (b *TxBuilder) publishAndLog(ctx context.Context,
	skipBroadcast bool) (*AssetPacket, error) {

	if advanced, ok := b.walletKit.(CustomAnchorWalletKitClient); ok {
		return advanced.PublishAndLogTransferWithRequest(
			ctx, &PublishAndLogTransferRequest{
				AnchorPsbt:            b.anchorPsbt,
				VirtualPsbts:          [][]byte{b.signedPsbt},
				PassiveAssetPsbts:     b.passivePsbts,
				ChangeOutputIndex:     b.changeOutputIndex,
				LockedUTXOs:           b.lockedUTXOs,
				SkipAnchorTxBroadcast: skipBroadcast,
			},
		)
	}

	return b.walletKit.PublishAndLogTransfer(
		ctx, b.anchorPsbt, [][]byte{b.signedPsbt}, b.passivePsbts,
		skipBroadcast,
	)
}

func (b *TxBuilder) commitVirtualPsbts(ctx context.Context) (
	*CommitVirtualPsbtsResponse, error) {

	virtualPsbts := [][]byte{b.signedPsbt}
	advanced, ok := b.walletKit.(CustomAnchorWalletKitClient)
	if !ok {
		legacy, err := b.walletKit.CommitVirtualPsbts(
			ctx, virtualPsbts, b.passivePsbts, b.feeRate,
		)
		if err != nil {
			return nil, err
		}

		return &CommitVirtualPsbtsResponse{
			AnchorPsbt:        legacy.AnchorPsbt,
			VirtualPsbts:      legacy.VirtualPsbts,
			PassiveAssetPsbts: legacy.PassiveAssetPsbts,
			ChangeOutputIndex: -1,
		}, nil
	}

	anchorPsbt, err := anchor.PreparePsbt(virtualPsbts, b.passivePsbts)
	if err != nil {
		return nil, err
	}

	return advanced.CommitVirtualPsbtsWithRequest(
		ctx, &CommitVirtualPsbtsRequest{
			AnchorPsbt:        anchorPsbt,
			VirtualPsbts:      virtualPsbts,
			PassiveAssetPsbts: b.passivePsbts,
			Funding: AnchorFundingPlan{
				ChangeOutput: AnchorChangeOutput{
					Mode: AnchorChangeOutputAdd,
				},
				Fee: AnchorFee{
					Mode:    AnchorFeeSatPerVByte,
					FeeRate: b.feeRate,
				},
			},
		},
	)
}

func (b *TxBuilder) applyCommitResponse(resp *CommitVirtualPsbtsResponse) {
	b.anchorPsbt = append([]byte(nil), resp.AnchorPsbt...)
	b.changeOutputIndex = resp.ChangeOutputIndex
	b.lockedUTXOs = append([]Outpoint(nil), resp.LockedUTXOs...)

	if len(resp.VirtualPsbts) > 0 {
		b.signedPsbt = append([]byte(nil), resp.VirtualPsbts[0]...)
	}
	if len(resp.PassiveAssetPsbts) > 0 {
		b.passivePsbts = clone2Dimensional(resp.PassiveAssetPsbts)
	}
}

func committedTransferFromResponse(
	resp *CommitVirtualPsbtsResponse) *CommittedTransfer {

	return &CommittedTransfer{
		AnchorPsbt:        resp.AnchorPsbt,
		VirtualPsbts:      resp.VirtualPsbts,
		PassiveAssetPsbts: resp.PassiveAssetPsbts,
	}
}

func (b *TxBuilder) signAnchor(ctx context.Context) error {
	if b.anchorSigner == nil {
		return nil
	}

	signedAnchorPsbt, err := b.anchorSigner(ctx, b.anchorPsbt)
	if err != nil {
		return err
	}

	b.anchorPsbt = append([]byte(nil), signedAnchorPsbt...)
	return nil
}

// clone2Dimensional performs a deep copy of a slice of byte slices.
func clone2Dimensional(src [][]byte) [][]byte {
	if src == nil {
		return nil
	}

	clone := make([][]byte, len(src))
	for i := range src {
		clone[i] = append([]byte(nil), src[i]...)
	}

	return clone
}
