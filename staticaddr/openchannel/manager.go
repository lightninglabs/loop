package openchannel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	serverrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

const (
	// The minimum number of confirmations lnd requires inputs to have for a
	// channel opening.
	defaultUtxoMinConf = 1

	// Is the default confirmation target for a channel open transaction.
	defaultConfTarget int32 = 3
)

var (
	ErrOpeningChannelUnavailableDeposits = errors.New("some deposits are " +
		"not usable to open a channel with")

	// errPsbtFinalized is returned when the PSBT finalize step was already
	// sent to lnd. After this point the funding transaction may have been
	// broadcast, so deposits must not be rolled back to Deposited.
	errPsbtFinalized = errors.New("PSBT finalize already sent")
)

// Config is the configuration struct for the open channel manager.
type Config struct {
	// StaticAddressServerClient is the client that calls the swap server
	// rpcs to negotiate static address withdrawals.
	Server serverrpc.StaticAddressServerClient

	// AddressManager gives the withdrawal manager access to static address
	// parameters.
	AddressManager AddressManager

	// DepositManager gives the withdrawal manager access to the deposits
	// enabling it to create and manage withdrawals.
	DepositManager DepositManager

	// WithdrawalManager is used to create the withdrawal transaction into
	// the channel funding address.
	WithdrawalManager WithdrawalManager

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient

	// LightningClient is the lnd client that is used to open channels.
	LightningClient lndclient.LightningClient
}

type newOpenChannelRequest struct {
	request  *lnrpc.OpenChannelRequest
	respChan chan *newOpenChannelResponse
}

type newOpenChannelResponse struct {
	// ChanTxHash is the transaction hash of the channel open transaction.
	ChanTxHash *chainhash.Hash

	// Err is the error that occurred during the channel open process.
	err error
}

// Manager is the main struct that handles the open channel manager.
type Manager struct {
	cfg *Config

	newOpenChannelRequestChan chan newOpenChannelRequest

	// exitChan signals subroutines that the open channel is exiting.
	exitChan chan struct{}

	// errChan forwards errors from the open channel to the server.
	errChan chan error
}

// NewManager creates a new manager instance.
func NewManager(cfg *Config) *Manager {
	m := &Manager{
		cfg:                       cfg,
		exitChan:                  make(chan struct{}),
		newOpenChannelRequestChan: make(chan newOpenChannelRequest),
		errChan:                   make(chan error),
	}

	return m
}

// Run runs the open channel manager.
func (m *Manager) Run(ctx context.Context) error {
	err := m.recoverOpeningChannelDeposits(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case req := <-m.newOpenChannelRequestChan:
			chanTxHash, err := m.OpenChannel(ctx, req.request)
			resp := &newOpenChannelResponse{
				ChanTxHash: chanTxHash,
				err:        err,
			}

			select {
			case req.respChan <- resp:

			case <-ctx.Done():
				// Notify subroutines that the main loop has
				// been canceled.
				close(m.exitChan)

				return ctx.Err()
			}

		case <-ctx.Done():
			// Signal subroutines that the manager is exiting.
			close(m.exitChan)

			return ctx.Err()
		}
	}
}

// recoverOpeningChannelDeposits resolves deposits that were left in
// OpeningChannel after a client restart. If a deposit input is still unspent,
// the channel open did not publish and we move back to Deposited. If the input
// is no longer unspent, it was spent on-chain and we finalize it as
// ChannelPublished.
func (m *Manager) recoverOpeningChannelDeposits(ctx context.Context) error {
	openingDeposits, err := m.cfg.DepositManager.GetActiveDepositsInState(
		deposit.OpeningChannel,
	)
	if err != nil {
		return fmt.Errorf("unable to fetch opening channel deposits: %w",
			err)
	}

	if len(openingDeposits) == 0 {
		return nil
	}

	log.Infof("Recovering %d deposits in OpeningChannel state",
		len(openingDeposits))

	utxos, err := m.cfg.WalletKit.ListUnspent(
		ctx, 0, 0,
	)
	if err != nil {
		return fmt.Errorf("unable to list unspent outputs for recovery: %w",
			err)
	}

	unspentOutpoints := make(map[wire.OutPoint]struct{}, len(utxos))
	for _, utxo := range utxos {
		unspentOutpoints[utxo.OutPoint] = struct{}{}
	}

	var (
		deposited        []*deposit.Deposit
		channelPublished []*deposit.Deposit
	)

	for _, d := range openingDeposits {
		_, stillUnspent := unspentOutpoints[d.OutPoint]
		if stillUnspent {
			deposited = append(deposited, d)
			continue
		}

		channelPublished = append(channelPublished, d)
	}

	if len(deposited) > 0 {
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, deposited, fsm.OnError, deposit.Deposited,
		)
		if err != nil {
			return fmt.Errorf("unable to recover unspent opening "+
				"deposits: %w", err)
		}
	}

	if len(channelPublished) > 0 {
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, channelPublished, deposit.OnChannelPublished,
			deposit.ChannelPublished,
		)
		if err != nil {
			return fmt.Errorf("unable to recover spent opening "+
				"deposits: %w", err)
		}
	}

	log.Infof("Recovered opening channel deposits: %d returned to Deposited, "+
		"%d marked ChannelPublished", len(deposited),
		len(channelPublished))

	return nil
}

// OpenChannel transitions the requested deposits into the OpeningChannel state
// and then starts the open channel psbt flow between the client's lnd instance
// and the server.
func (m *Manager) OpenChannel(ctx context.Context,
	req *lnrpc.OpenChannelRequest) (*chainhash.Hash, error) {

	var (
		outpoints []wire.OutPoint
		deposits  []*deposit.Deposit
		allActive bool
		feeRate   chainfee.SatPerKWeight
		err       error
	)

	if req.LocalFundingAmount == 0 && !req.FundMax {
		return nil, fmt.Errorf("either local funding amount or " +
			"fundmax must be set")
	}

	if req.LocalFundingAmount != 0 && req.FundMax {
		return nil, fmt.Errorf("local funding amount and fundmax " +
			"cannot be set at the same time")
	}

	// Validate PSBT-incompatible flags early, before locking deposits.
	// We accept MinConfs=0 here because that's the proto default for unset
	// values and normalize to defaultUtxoMinConf later.
	if err := validateInitialPsbtFlags(req); err != nil {
		return nil, err
	}

	// Determine the commitment type for the channel.
	chanCommitmentType, err := resolveCommitmentType(req.CommitmentType)
	if err != nil {
		return nil, err
	}

	// Estimate the fee rate before deposit selection so that we can verify
	// the selected deposits cover the funding amount plus fees.
	if req.SatPerVbyte == 0 {
		feeRate, err = m.cfg.WalletKit.EstimateFeeRate(
			ctx, defaultConfTarget,
		)
		if err != nil {
			return nil, fmt.Errorf("error estimating fee rate: %w",
				err)
		}
	} else {
		feeRate = chainfee.SatPerKVByte(
			req.SatPerVbyte * 1000,
		).FeePerKWeight()
	}

	// There are three ways in which we select deposits to open a channel
	// with. 1.) The user manually selects the deposits. 2.) The user only
	// selects a local channel amount in which case we coin-select deposits
	// to cover for it. 3.) The user selects the fundmax flag, in which case
	// we select all deposits to fund the channel.
	if len(req.Outpoints) > 0 {
		// Ensure that the deposits are in a state in which they are
		// available for a channel open.
		outpoints, err = staticutil.ToWireOutpoints(req.Outpoints)
		if err != nil {
			return nil, fmt.Errorf("error parsing outpoints: %w",
				err)
		}

		deposits, allActive =
			m.cfg.DepositManager.AllOutpointsActiveDeposits(
				outpoints, deposit.Deposited,
			)
		if !allActive {
			return nil, ErrOpeningChannelUnavailableDeposits
		}
	} else {
		// We have to select the deposits that are used to fund the
		// channel.
		deposits, err = m.cfg.DepositManager.GetActiveDepositsInState(
			deposit.Deposited,
		)
		if err != nil {
			return nil, err
		}

		if req.LocalFundingAmount != 0 {
			deposits, err = staticutil.SelectDeposits(
				deposits, req.LocalFundingAmount,
				feeRate, chanCommitmentType,
			)
			if err != nil {
				return nil, fmt.Errorf("error selecting "+
					"deposits: %w", err)
			}
		} else {
			// The fundmax flag is set, hence we select all deposits
			// for funding the channel.
		}
	}

	// Pre-check: calculate the channel funding amount and the optional
	// change before locking deposits. This ensures the selected deposits
	// can cover the funding amount plus fees.
	chanFundingAmt, _, calcErr := withdraw.CalculateWithdrawalTxValues(
		deposits, btcutil.Amount(req.LocalFundingAmount), feeRate, nil,
		chanCommitmentType,
	)
	if calcErr != nil {
		return nil, fmt.Errorf("error calculating funding tx "+
			"values: %w", calcErr)
	}

	// We need to transition the deposits to the opening channel state
	// before we start the channel open process. This is important to
	// ensure that the deposits are not used for other purposes while we
	// are opening the channel.
	err = m.cfg.DepositManager.TransitionDeposits(
		ctx, deposits, deposit.OnOpeningChannel, deposit.OpeningChannel,
	)
	if err != nil {
		return nil, err
	}

	openChanRequest := &lnrpc.OpenChannelRequest{
		NodePubkey:                 req.NodePubkey,
		LocalFundingAmount:         int64(chanFundingAmt),
		PushSat:                    req.PushSat,
		Private:                    req.Private,
		MinHtlcMsat:                req.MinHtlcMsat,
		RemoteCsvDelay:             req.RemoteCsvDelay,
		MinConfs:                   defaultUtxoMinConf,
		SpendUnconfirmed:           false,
		CloseAddress:               req.CloseAddress,
		RemoteMaxValueInFlightMsat: req.RemoteMaxValueInFlightMsat,
		RemoteMaxHtlcs:             req.RemoteMaxHtlcs,
		MaxLocalCsv:                req.MaxLocalCsv,
		CommitmentType:             chanCommitmentType,
		ZeroConf:                   req.ZeroConf,
		ScidAlias:                  req.ScidAlias,
		BaseFee:                    req.BaseFee,
		FeeRate:                    req.FeeRate,
		UseBaseFee:                 req.UseBaseFee,
		UseFeeRate:                 req.UseFeeRate,
		RemoteChanReserveSat:       req.RemoteChanReserveSat,
		Memo:                       req.Memo,
	}

	chanTxHash, err := m.openChannelPsbt(
		ctx, openChanRequest, deposits, feeRate,
	)
	if err != nil {
		log.Infof("error opening channel: %v", err)

		// If the PSBT was already finalized and sent to lnd, the
		// funding transaction may have been broadcast. In that case
		// we must not roll back the deposits to Deposited as they
		// may already be spent on-chain.
		if !errors.Is(err, errPsbtFinalized) {
			err2 := m.cfg.DepositManager.TransitionDeposits(
				ctx, deposits, fsm.OnError,
				deposit.Deposited,
			)
			if err2 != nil {
				log.Errorf("failed transitioning deposits "+
					"after failed channel open: %v",
					err2)
			}
		}

		return nil, err
	}

	return chanTxHash, nil
}

// openChannelPsbt starts an interactive channel open protocol that uses a
// partially signed bitcoin transaction (PSBT) to fund the channel output. The
// protocol involves several steps between the loop client and the server:
//
// RPC server                           CLI client
//
//	|                                    |
//	|  |<------open channel (stream)-----|
//	|  |-------ready for funding----->|  |
//	|  |------------------------------|  | create psbt from deposits
//	|  |<------PSBT verify------------|  |
//	|  |-------ready for signing----->|  |
//	|  |------------------------------|  | request server co-sig
//	|  |------------------------------|  | sign psbt with combined sig
//	|  |<------PSBT finalize----------|  |
//	|  |-------channel pending------->|  |
//	|  |-------channel open------------->|
//	|                                    |
func (m *Manager) openChannelPsbt(ctx context.Context,
	req *lnrpc.OpenChannelRequest, deposits []*deposit.Deposit,
	feeRate chainfee.SatPerKWeight) (*chainhash.Hash, error) {

	var (
		pendingChanID [32]byte
		shimPending   = true
		psbtFinalized bool
		basePsbtBytes []byte
		quit          = make(chan struct{})
		srvMsg        = make(chan *lnrpc.OpenStatusUpdate, 1)
		srvErr        = make(chan error, 1)
	)

	// Make sure the user didn't supply any command line flags that are
	// incompatible with PSBT funding.
	err := checkPsbtFlags(req)
	if err != nil {
		return nil, err
	}

	// Generate a new, random pending channel ID that we'll use as the main
	// identifier when sending update messages to the RPC server.
	if _, err := rand.Read(pendingChanID[:]); err != nil {
		return nil, fmt.Errorf("unable to generate random chan ID: "+
			"%w", err)
	}
	log.Infof("Starting PSBT funding flow with pending channel ID %x.\n",
		pendingChanID)

	// maybeCancelShim is a helper function that cancels the funding shim
	// with the RPC server in case we end up aborting early.
	maybeCancelShim := func() {
		// If the user canceled while there was still a shim registered
		// with the wallet, release the resources now.
		if shimPending {
			log.Infof("Canceling PSBT funding flow for pending "+
				"channel ID %x.\n", pendingChanID)

			cancelMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_ShimCancel{
					ShimCancel: &lnrpc.FundingShimCancel{
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err := m.cfg.LightningClient.FundingStateStep(
				ctx, cancelMsg,
			)
			if err != nil {
				log.Errorf("Error canceling shim: %v\n", err)
			}
			shimPending = false
		}
	}
	defer maybeCancelShim()

	// Create the PSBT funding shim that will tell the funding manager we
	// want to use a PSBT.
	req.FundingShim = &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_PsbtShim{
			PsbtShim: &lnrpc.PsbtShim{
				PendingChanId: pendingChanID[:],
				BasePsbt:      basePsbtBytes,
				// Setting this to false since we don't batch
				// open channels.
				NoPublish: false,
			},
		},
	}

	// Start the interactive process by opening the stream connection to the
	// daemon. If the user cancels by pressing <Ctrl+C> we need to cancel
	// the shim. To not just kill the process on interrupt, we need to
	// explicitly capture the signal.
	rawCtx, _, rawClient := m.cfg.LightningClient.RawClientWithMacAuth(ctx)
	stream, err := rawClient.OpenChannel(rawCtx, req)
	if err != nil {
		return nil, fmt.Errorf("opening stream to server "+
			"failed: %w", err)
	}

	// We also need to spawn a goroutine that reads from the server. This
	// will copy the messages to the channel as long as they come in or add
	// exactly one error to the error stream and then bail out.
	go func() {
		for {
			// Recv blocks until a message or error arrives.
			resp, err := stream.Recv()
			if err == io.EOF {
				srvErr <- fmt.Errorf("loop shutting down: %w",
					err)

				return
			} else if err != nil {
				srvErr <- fmt.Errorf("got error from server: "+
					"%v", err)

				return
			}

			// Don't block on sending in case of shutting down.
			select {
			case srvMsg <- resp:
			case <-quit:

				return
			}
		}
	}()

	// Spawn another goroutine that only handles loop server shutdown or
	// errors from the lnd server. Both will trigger an attempt to cancel
	// the shim with the server.
	go func() {
		select {
		case <-ctx.Done():
			log.Infof("OpenChannel context cancel.")
			close(quit)

		case err := <-srvErr:
			log.Errorf("OpenChannel lnd server error received: "+
				"%v\n", err)

			// If the remote peer canceled on us, the reservation
			// has already been deleted. We don't need to try to
			// remove it again, this would just produce another
			// error.
			cancelErr := chanfunding.ErrRemoteCanceled.Error()
			if err != nil && strings.Contains(
				err.Error(), cancelErr,
			) {

				shimPending = false
			}
			close(quit)

		case <-quit:
		}
	}()

	for {
		var srvResponse *lnrpc.OpenStatusUpdate
		select {
		case srvResponse = <-srvMsg:
		case <-quit:
			cancelErr := fmt.Errorf("open channel flow canceled")
			if psbtFinalized {
				return nil, fmt.Errorf("%w: %v",
					errPsbtFinalized, cancelErr)
			}
			return nil, cancelErr
		}

		switch update := srvResponse.Update.(type) {
		case *lnrpc.OpenStatusUpdate_PsbtFund:
			fundingAmount := update.PsbtFund.FundingAmount
			if req.LocalFundingAmount != fundingAmount {
				err := fmt.Errorf("funding amount "+
					"%v doesn't match local "+
					"funding amount %v",
					fundingAmount,
					req.LocalFundingAmount)

				return nil, err
			}

			addr := update.PsbtFund.FundingAddress

			log.Infof("PSBT funding initiated with peer "+
				"%x, funding amount %v, funding "+
				"address %v", req.NodePubkey,
				fundingAmount, addr)

			// Create the psbt funding transaction for the
			// channel. Ensure the selected deposits amount
			// to the psbt funding amount.
			channelFundingAddress, err := btcutil.DecodeAddress(
				addr, m.cfg.ChainParams,
			)
			if err != nil {
				return nil, fmt.Errorf("decoding funding "+
					"address: %w", err)
			}

			//nolint:ll
			signedTx, unsignedPsbt, err := m.cfg.WithdrawalManager.CreateFinalizedWithdrawalTx(
				ctx, deposits, channelFundingAddress, feeRate,
				fundingAmount, req.CommitmentType,
			)
			if err != nil {
				return nil, fmt.Errorf("creating PSBT "+
					"failed: %w", err)
			}

			// Verify that the psbt contains the correct outputs.
			verifyMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
					PsbtVerify: &lnrpc.FundingPsbtVerify{
						FundedPsbt:    unsignedPsbt,
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err = m.cfg.LightningClient.FundingStateStep(
				ctx, verifyMsg,
			)
			if err != nil {
				return nil, fmt.Errorf("verifying PSBT by lnd "+
					"failed: %v", err)
			}

			// Now that we have the final transaction, we can
			// finalize the PSBT and publish the channel open
			// transaction.
			var buffer bytes.Buffer
			err = signedTx.Serialize(&buffer)
			if err != nil {
				return nil, fmt.Errorf("error serializing "+
					"tx: %w", err)
			}
			transitionMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
					PsbtFinalize: &lnrpc.FundingPsbtFinalize{
						FinalRawTx:    buffer.Bytes(),
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err = m.cfg.LightningClient.FundingStateStep(
				ctx, transitionMsg,
			)
			if err != nil {
				return nil, fmt.Errorf("finalizing PSBT "+
					"funding flow failed: %v", err)
			}

			// The finalize step succeeded. From this point
			// on the funding tx may have been broadcast, so
			// deposits must not be rolled back.
			psbtFinalized = true

		case *lnrpc.OpenStatusUpdate_ChanPending:
			// As soon as the channel is pending, there is no more
			// shim that needs to be canceled. If the user
			// interrupts now, we don't need to clean up anything.
			shimPending = false

			hash, err := chainhash.NewHash(
				update.ChanPending.Txid,
			)
			if err != nil {
				log.Infof("Error creating hash for channel "+
					"open tx: %v", err)
			}

			log.Infof("Channel transaction pending: %v",
				hash.String())
			log.Infof("Please monitor the channel from lnd")

			err = m.cfg.DepositManager.TransitionDeposits(
				ctx, deposits, deposit.OnChannelPublished,
				deposit.ChannelPublished,
			)
			if err != nil {
				log.Errorf("error transitioning deposits to "+
					"ChannelPublished: %v", err)
			}

			// We can now close the quit channel to stop the
			// goroutine that reads from the server.
			close(quit)

			// Nil indicates that the channel was successfully
			// published.
			return hash, nil
		}
	}
}

// validateInitialPsbtFlags validates request fields that are incompatible with
// the interactive PSBT channel funding flow.
func validateInitialPsbtFlags(req *lnrpc.OpenChannelRequest) error {
	if req.MinConfs != 0 && req.MinConfs != defaultUtxoMinConf {
		return fmt.Errorf("custom MinConfs not supported for PSBT " +
			"funding, only the default is allowed")
	}

	if req.SpendUnconfirmed {
		return fmt.Errorf("SpendUnconfirmed is not supported " +
			"for PSBT funding")
	}

	return nil
}

// resolveCommitmentType validates supported channel commitment types and
// normalizes unknown/default to STATIC_REMOTE_KEY.
func resolveCommitmentType(commitmentType lnrpc.CommitmentType) (
	lnrpc.CommitmentType, error) {

	switch commitmentType {
	case lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
		lnrpc.CommitmentType_STATIC_REMOTE_KEY:

		return lnrpc.CommitmentType_STATIC_REMOTE_KEY, nil

	case lnrpc.CommitmentType_ANCHORS:
		return lnrpc.CommitmentType_ANCHORS, nil

	case lnrpc.CommitmentType_SIMPLE_TAPROOT:
		return lnrpc.CommitmentType_SIMPLE_TAPROOT, nil

	default:
		return lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE, fmt.Errorf(
			"unsupported commitment type %v", commitmentType,
		)
	}
}

// checkPsbtFlags make sure a request to open a channel doesn't set any
// parameters that are incompatible with the PSBT funding flow.
func checkPsbtFlags(req *lnrpc.OpenChannelRequest) error {
	if req.MinConfs != defaultUtxoMinConf || req.SpendUnconfirmed {
		return fmt.Errorf("specifying minimum confirmations for PSBT " +
			"funding is not supported")
	}
	if req.TargetConf != 0 || req.SatPerByte != 0 || req.SatPerVbyte != 0 { // nolint:staticcheck
		return fmt.Errorf("setting fee estimation parameters not " +
			"supported for PSBT funding")
	}
	return nil
}

// DeliverOpenChannelRequest forwards a open channel request to the manager main
// loop.
func (m *Manager) DeliverOpenChannelRequest(ctx context.Context,
	req *lnrpc.OpenChannelRequest) (*chainhash.Hash, error) {

	request := newOpenChannelRequest{
		request:  req,
		respChan: make(chan *newOpenChannelResponse),
	}

	// Send the open channel request to the manager run loop.
	select {
	case m.newOpenChannelRequestChan <- request:

	case <-m.exitChan:
		return nil, fmt.Errorf("open channel manager has been " +
			"canceled")

	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled while opening " +
			"channel")
	}

	// Wait for the response from the manager run loop.
	select {
	case resp := <-request.respChan:
		return resp.ChanTxHash, resp.err

	case <-m.exitChan:
		return nil, fmt.Errorf("open channel manager has been " +
			"canceled")

	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled while waiting " +
			"for open channel response")
	}
}
