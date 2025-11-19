package openchannel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	serverrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
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
	request  *looprpc.OpenChannelRequest
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

	// initChan signals the daemon that the openchannel manager has
	// completed its initialization.
	initChan chan struct{}

	newOpenChannelRequestChan chan newOpenChannelRequest

	// exitChan signals subroutines that the withdrawal manager is exiting.
	exitChan chan struct{}

	// errChan forwards errors from the withdrawal manager to the server.
	errChan chan error
}

// NewManager creates a new manager instance.
func NewManager(cfg *Config) *Manager {
	m := &Manager{
		cfg:                       cfg,
		initChan:                  make(chan struct{}),
		exitChan:                  make(chan struct{}),
		newOpenChannelRequestChan: make(chan newOpenChannelRequest),
		errChan:                   make(chan error),
	}

	return m
}

// Run runs the open channel manager.
func (m *Manager) Run(ctx context.Context) error {
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

// OpenChannel transitions the requested deposits into the OpeningChannel state
// and then starts the open channel psbt flow between the client's lnd instance
// and the server.
func (m *Manager) OpenChannel(ctx context.Context,
	req *looprpc.OpenChannelRequest) (*chainhash.Hash, error) {

	var (
		outpoints []wire.OutPoint
		deposits  []*deposit.Deposit
		allActive bool
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

	// There are three ways in which we select deposits to open a channel
	// with. 1.) The user manually selects the deposits. 2.) The user only
	// selects a local channel amount in which case we coin-select deposits
	// to cover for it. 3.) The user selects the fundmax flag, in which case
	// we select all deposits to fund the channel.
	if len(req.Outpoints) > 0 {
		// Ensure that the deposits are in a state in which they are
		// available for a channel open.
		outpoints, err = toServerOutpoints(req.Outpoints)
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

	// Calculate the channel funding amount and the optional change based on
	// the selected deposits and user provided channel parameters.
	chanFundingAmt, changeAmt, calcErr := calculateFundingTxValaues(
		ctx, deposits, btcutil.Amount(req.LocalFundingAmount),
		req.FundMax, req.SatPerVbyte, req.CommitmentType,
		m.cfg.WalletKit,
	)
	if calcErr != nil {
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, deposits, fsm.OnError, deposit.Deposited,
		)
		log.Errorf("failed transitioning deposits: %v", err)

		return nil, fmt.Errorf("error calculating funding tx "+
			"values: %w", calcErr)
	}

	chanCommitmentType := lnrpc.CommitmentType_STATIC_REMOTE_KEY
	switch req.CommitmentType {
	case looprpc.CommitmentType_ANCHORS:
		chanCommitmentType = lnrpc.CommitmentType_ANCHORS

	case looprpc.CommitmentType_SIMPLE_TAPROOT:
		chanCommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	openChanRequest := &lnrpc.OpenChannelRequest{
		LocalFundingAmount:         int64(chanFundingAmt),
		NodePubkey:                 req.NodePubkey,
		PushSat:                    req.PushSat,
		Private:                    req.Private,
		MinHtlcMsat:                req.MinHtlcMsat,
		RemoteCsvDelay:             req.RemoteCsvDelay,
		MinConfs:                   defaultUtxoMinConf,
		SpendUnconfirmed:           req.SpendUnconfirmed,
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
		ctx, openChanRequest, deposits, int64(changeAmt),
	)
	if err != nil {
		log.Infof("error opening channel: %v", err)
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, deposits, fsm.OnError, deposit.Deposited,
		)
		if err != nil {
			return nil, err
		}
	}

	return chanTxHash, nil
}

func toServerOutpoints(outpoints []*looprpc.OutPoint) ([]wire.OutPoint,
	error) {

	var serverOutpoints []wire.OutPoint
	for _, o := range outpoints {
		outpointStr := fmt.Sprintf("%s:%d", o.TxidStr, o.OutputIndex)
		newOutpoint, err := wire.NewOutPointFromString(outpointStr)
		if err != nil {
			return nil, err
		}

		serverOutpoints = append(serverOutpoints, *newOutpoint)
	}

	return serverOutpoints, nil
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
	changeAmt int64) (*chainhash.Hash, error) {

	var (
		pendingChanID [32]byte
		shimPending   = true
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
			return nil, fmt.Errorf("open channel flow canceled")
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

			addrParams, err :=
				m.cfg.AddressManager.GetStaticAddressParameters(
					ctx,
				)
			if err != nil {
				return nil, err
			}

			staticAddress, err :=
				m.cfg.AddressManager.GetStaticAddress(
					ctx,
				)
			if err != nil {
				return nil, err
			}

			tx, psbtBytes, err := m.createFundingPsbt(
				deposits, fundingAmount, changeAmt,
				channelFundingAddress, addrParams.PkScript,
			)
			if err != nil {
				return nil, fmt.Errorf("creating PSBT "+
					"failed: %w", err)
			}

			// Verify that the psbt contains the correct outputs.
			verifyMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
					PsbtVerify: &lnrpc.FundingPsbtVerify{
						FundedPsbt:    psbtBytes,
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

			// Now that we know the PSBT looks good we can request
			// a cooperative signature from the server.
			//
			// Create a musig2 session for each deposit.
			sessions, clientNonces, idx, err :=
				staticutil.CreateMusig2SessionsPerDeposit(
					ctx, m.cfg.Signer, deposits, addrParams,
					staticAddress,
				)
			if err != nil {
				return nil, fmt.Errorf("failed creating " +
					"session per deposit")
			}

			prevOuts, err := staticutil.ToPrevOuts(
				deposits, addrParams.PkScript,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to get "+
					"prevouts: %w", err)
			}

			sigReq := &serverrpc.SignOpenChannelPsbtRequest{
				OpenChannelTxPsbt: psbtBytes,
				DepositToNonces:   clientNonces,
				PrevoutInfo: staticutil.GetPrevoutInfo(
					prevOuts,
				),
			}

			sigResp, err := m.cfg.Server.SignOpenChannelPsbt(
				ctx, sigReq,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to sign open "+
					"channel psbt with the server: %w", err)
			}

			// Do some sanity checks.
			txHash := tx.TxHash()
			if !bytes.Equal(txHash.CloneBytes(), sigResp.Txid) {
				return nil, errors.New("txid doesn't match")
			}

			if len(sigResp.SigningInfo) != len(deposits) {
				return nil, errors.New("invalid number of " +
					"deposit signatures")
			}

			prevOutFetcher := txscript.NewMultiPrevOutFetcher(
				prevOuts,
			)

			sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

			// Create our digest.
			var sigHash [32]byte

			// We'll now add the nonce to our session and sign the
			// tx.
			for deposit, sigAndNonce := range sigResp.SigningInfo {
				session, ok := sessions[deposit]
				if !ok {
					return nil, errors.New("session not " +
						"found")
				}

				nonce := [musig2.PubNonceSize]byte{}
				copy(nonce[:], sigAndNonce.Nonce)
				haveAllNonces, err :=
					m.cfg.Signer.MuSig2RegisterNonces(
						ctx, session.SessionID,
						[][musig2.PubNonceSize]byte{nonce},
					)
				if err != nil {
					return nil, fmt.Errorf("error "+
						"registering nonces: %w", err)
				}

				if !haveAllNonces {
					return nil, errors.New("expected all " +
						"nonces to be registered")
				}

				taprootSigHash, err := txscript.CalcTaprootSignatureHash(
					sigHashes, txscript.SigHashDefault, tx,
					idx[deposit], prevOutFetcher,
				)
				if err != nil {
					return nil, fmt.Errorf("error "+
						"calculating taproot sig "+
						"hash: %w", err)
				}

				copy(sigHash[:], taprootSigHash)

				// Sign the tx.
				_, err = m.cfg.Signer.MuSig2Sign(
					ctx, session.SessionID, sigHash, false,
				)
				if err != nil {
					return nil, fmt.Errorf("error signing "+
						"tx: %w", err)
				}

				// Combine the signature with the client signature.
				haveAllSigs, sig, err := m.cfg.Signer.MuSig2CombineSig(
					ctx, session.SessionID,
					[][]byte{sigAndNonce.Sig},
				)
				if err != nil {
					return nil, fmt.Errorf("error "+
						"combining signature: %w", err)
				}

				if !haveAllSigs {
					return nil, errors.New("expected all " +
						"signatures to be " +
						"combined")
				}

				tx.TxIn[idx[deposit]].Witness = wire.TxWitness{
					sig,
				}
			}

			// Now that we have the final transaction, we can
			// finalize the PSBT and publish the channel open
			// transaction.
			var buffer bytes.Buffer
			err = tx.Serialize(&buffer)
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

// createFundingPsbt creates the unsigned channel funding transaction and psbt
// packet.
func (m *Manager) createFundingPsbt(deposits []*deposit.Deposit,
	fundingAmount int64, changeAmount int64, fundingAddress btcutil.Address,
	staticAddressPkScript []byte) (*wire.MsgTx, []byte, error) {

	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order for the server
	// signed them.
	for _, d := range deposits {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: d.OutPoint,
			Witness:          [][]byte{},
		})
	}

	// Check that the sum of deposit outpoints is equal to the swap amount.
	fundingPkScript, err := txscript.PayToAddrScript(fundingAddress)
	if err != nil {
		return nil, nil, err
	}

	// Create the channel output.
	fundingOutput := &wire.TxOut{
		Value:    fundingAmount,
		PkScript: fundingPkScript,
	}
	msgTx.AddTxOut(fundingOutput)

	// Check if we need to add a change output.
	if changeAmount > 0 {
		changeOutput := &wire.TxOut{
			Value:    changeAmount,
			PkScript: staticAddressPkScript,
		}
		msgTx.AddTxOut(changeOutput)
	}

	psbtx, err := psbt.NewFromUnsignedTx(msgTx)
	if err != nil {
		return nil, nil, err
	}

	pInputs := make([]psbt.PInput, len(deposits))
	for i, d := range deposits {
		pInputs[i] = psbt.PInput{
			WitnessUtxo: &wire.TxOut{
				Value:    int64(d.Value),
				PkScript: staticAddressPkScript,
			},
		}
	}
	psbtx.Inputs = pInputs

	// Serialize the psbt to send it to the client.
	var psbtBuf bytes.Buffer
	err = psbtx.Serialize(&psbtBuf)
	if err != nil {
		return nil, nil, err
	}

	return msgTx, psbtBuf.Bytes(), nil
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
	req *looprpc.OpenChannelRequest) (*chainhash.Hash, error) {

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

func calculateFundingTxValaues(ctx context.Context, deposits []*deposit.Deposit,
	localAmount btcutil.Amount, fundMax bool, satPerVbyte uint64,
	commitmentType looprpc.CommitmentType, feeEstimator Estimator) (
	btcutil.Amount, btcutil.Amount, error) {

	var (
		chanOpenFeeRate chainfee.SatPerKWeight
		err             error
		chanFundingAmt  btcutil.Amount
		changeAmount    btcutil.Amount
		dustLimit       = lnwallet.DustLimitForSize(input.P2TRSize)
	)

	if satPerVbyte == 0 {
		// Get the fee rate for the withdrawal sweep.
		chanOpenFeeRate, err = feeEstimator.EstimateFeeRate(
			ctx, defaultConfTarget,
		)
		if err != nil {
			return 0, 0, err
		}
	} else {
		chanOpenFeeRate = chainfee.SatPerKVByte(
			satPerVbyte * 1000,
		).FeePerKWeight()
	}

	totalDepositAmount := btcutil.Amount(0)
	for _, d := range deposits {
		totalDepositAmount += d.Value
	}

	// Estimate the open channel transaction fee without change.
	hasChange := false
	weight := chanOpenTxWeight(len(deposits), hasChange, commitmentType)
	feeWithoutChange := chanOpenFeeRate.FeeForWeight(weight)

	// If the user selected a local amount for the channel, check if a
	// change output is needed.
	if localAmount > 0 {
		// Estimate the transaction weight with change.
		hasChange = true
		weightWithChange := chanOpenTxWeight(
			len(deposits), hasChange, commitmentType,
		)
		feeWithChange := chanOpenFeeRate.FeeForWeight(weightWithChange)

		// The available change that can cover fees is the total
		// selected deposit amount minus the local channel amount.
		change := totalDepositAmount - localAmount

		switch {
		case change-feeWithChange >= dustLimit:
			// If the change can cover the fees without turning into
			// dust, add a non-dust change output.
			changeAmount = change - feeWithChange
			chanFundingAmt = localAmount

		case change-feeWithoutChange >= 0:
			// If the change is dust, we give it to the miners.
			chanFundingAmt = localAmount

		default:
			// If the fees eat into our local channel amount, we
			// fail opening the channel.
			return 0, 0, fmt.Errorf("the change doesn't " +
				"cover for fees. Consider lowering the fee " +
				"rate or decrease the local amount")
		}
	} else if fundMax {
		// If the user wants to open the channel with the total value of
		// deposits, we don't need a change output.
		chanFundingAmt = totalDepositAmount - feeWithoutChange
	}

	if changeAmount < 0 {
		return 0, 0, fmt.Errorf("change amount is negative")
	}

	// Ensure that the channel funding amount is at least in the amount of
	// lnd's minimum channel size.
	if chanFundingAmt < funding.MinChanFundingSize {
		return 0, 0, fmt.Errorf("channel funding amount %v is lower "+
			"than the minimum channel funding size %v",
			chanFundingAmt, funding.MinChanFundingSize)
	}

	// For the users convenience we check that the change amount is lower
	// than each input's value. If the change amount is higher than an
	// input's value, we wouldn't have to include that input into the
	// transaction, saving fees.
	for _, d := range deposits {
		if changeAmount >= d.Value {
			return 0, 0, fmt.Errorf("change amount %v is "+
				"higher than an input value %v of input %v",
				changeAmount, d.Value, d.OutPoint.String())
		}
	}

	return chanFundingAmt, changeAmount, nil
}

// chanOpenTxWeight calculates the weight of a channel open transaction. The
// weight is calculated based on the number of deposit inputs, whether a change
// output is needed and the commitment type. The weight is used to estimate the
// transaction fee for the channel open transaction.
func chanOpenTxWeight(numInputs int, hasChange bool,
	commitmentType looprpc.CommitmentType) lntypes.WeightUnit {

	var weightEstimator input.TxWeightEstimator
	for i := 0; i < numInputs; i++ {
		weightEstimator.AddTaprootKeySpendInput(
			txscript.SigHashDefault,
		)
	}

	// Add the weight of the channel output.
	switch commitmentType {
	case looprpc.CommitmentType_SIMPLE_TAPROOT:
		weightEstimator.AddP2TROutput()

	default:
		weightEstimator.AddP2WSHOutput()
	}

	// If there's a change output add the weight of the static address.
	if hasChange {
		weightEstimator.AddP2TROutput()
	}

	return weightEstimator.Weight()
}
