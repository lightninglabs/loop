package openchannel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	"io"
	"strings"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	serverrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
)

const (
	defaultUtxoMinConf = 1
)

var (
	ErrOpeningChannelUnavailableDeposits = errors.New("some deposits are " +
		"not usable to open a channel with")
)

// Config ...
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

	LightningClient lndclient.LightningClient
}

// newOpenChannelRequest ...
type newOpenChannelRequest struct {
	request  *looprpc.OpenChannelRequest
	respChan chan *newOpenChannelResponse
}

// newOpenChannelResponse ...
type newOpenChannelResponse struct {
	txHash             string
	withdrawalPkScript string
	err                error
}

// Manager ...
type Manager struct {
	cfg *Config

	// initChan signals the daemon that the openchannel manager has
	// completed its initialization.
	initChan chan struct{}

	// newOpenChannelRequestChan ...
	newOpenChannelRequestChan chan newOpenChannelRequest

	// exitChan signals subroutines that the withdrawal manager is exiting.
	exitChan chan struct{}

	// errChan forwards errors from the withdrawal manager to the server.
	errChan chan error

	// initiationHeight stores the currently best known block height.
	initiationHeight atomic.Uint32
}

// NewManager ...
func NewManager(cfg *Config, currentHeight uint32) *Manager {
	m := &Manager{
		cfg:                       cfg,
		initChan:                  make(chan struct{}),
		exitChan:                  make(chan struct{}),
		newOpenChannelRequestChan: make(chan newOpenChannelRequest),
		errChan:                   make(chan error),
	}
	m.initiationHeight.Store(currentHeight)

	return m
}

// Run runs the open channel manager.
func (m *Manager) Run(ctx context.Context) error {
	newBlockChan, newBlockErrChan, err :=
		m.cfg.ChainNotifier.RegisterBlockEpochNtfn(ctx)

	if err != nil {
		return err
	}

	// Communicate to the caller that the manager has completed its
	// initialization.
	close(m.initChan)

	for {
		select {
		case <-newBlockChan:
			if err != nil {
				log.Errorf("Error republishing withdrawals: %v",
					err)
			}

		case req := <-m.newOpenChannelRequestChan:
			m.OpenChannel(ctx, req.request)
			resp := &newOpenChannelResponse{}

			select {
			case req.respChan <- resp:

			case <-ctx.Done():
				// Notify subroutines that the main loop has
				// been canceled.
				close(m.exitChan)

				return ctx.Err()
			}

		case err = <-newBlockErrChan:
			return err

		case <-ctx.Done():
			// Signal subroutines that the manager is exiting.
			close(m.exitChan)

			return ctx.Err()
		}
	}
}

// WaitInitComplete waits until the open channel manager has completed its
// setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address open channel manager initiation " +
		"complete.")

	<-m.initChan
}

// OpenChannel ...
func (m *Manager) OpenChannel(ctx context.Context,
	req *looprpc.OpenChannelRequest) error {

	// Ensure that the deposits are in a state in which they are available
	// for a channel open.
	outpoints, err := toServerOutpoints(req.Outpoints)
	if err != nil {
		return fmt.Errorf("error parsing outpoints: %w", err)
	}

	deposits, allActive := m.cfg.DepositManager.AllOutpointsActiveDeposits(
		outpoints, deposit.Deposited,
	)

	if !allActive {
		return ErrOpeningChannelUnavailableDeposits
	}

	err = m.cfg.DepositManager.TransitionDeposits(
		ctx, deposits, deposit.OnOpeningChannel, deposit.OpeningChannel,
	)
	if err != nil {
		return err
	}

	chanCommitmentType := lnrpc.CommitmentType_STATIC_REMOTE_KEY
	switch req.CommitmentType {
	case looprpc.CommitmentType_ANCHORS:
		chanCommitmentType = lnrpc.CommitmentType_ANCHORS

	case looprpc.CommitmentType_SIMPLE_TAPROOT:
		chanCommitmentType = lnrpc.CommitmentType_SIMPLE_TAPROOT
	}

	lnrpcOutpoints := make([]*lnrpc.OutPoint, len(req.Outpoints))
	for i, o := range req.Outpoints {
		lnrpcOutpoints[i] = &lnrpc.OutPoint{
			TxidStr:     o.TxidStr,
			OutputIndex: o.OutputIndex,
		}
	}

	openChanRequest := &lnrpc.OpenChannelRequest{
		SatPerVbyte:                req.SatPerVbyte,
		NodePubkey:                 req.NodePubkey,
		LocalFundingAmount:         req.LocalFundingAmount,
		PushSat:                    req.PushSat,
		Private:                    req.Private,
		MinHtlcMsat:                req.MinHtlcMsat,
		RemoteCsvDelay:             req.RemoteCsvDelay,
		MinConfs:                   req.MinConfs,
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
		Outpoints:                  lnrpcOutpoints,
	}

	err = m.openChannelPsbt(ctx, openChanRequest, deposits)
	if err != nil {
		err = m.cfg.DepositManager.TransitionDeposits(
			ctx, deposits, fsm.OnError, deposit.Deposited,
		)
		if err != nil {
			return err
		}
	}

	return err
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
// protocol involves several steps between the RPC server and the CLI client:
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
	req *lnrpc.OpenChannelRequest, deposits []*deposit.Deposit) error {

	var (
		pendingChanID [32]byte
		shimPending   = true
		basePsbtBytes []byte
		quit          = make(chan struct{})
		srvMsg        = make(chan *lnrpc.OpenStatusUpdate, 1)
		srvErr        = make(chan error, 1)
		ctxc, cancel  = context.WithCancel(ctx)
	)
	defer cancel()

	// Make sure the user didn't supply any command line flags that are
	// incompatible with PSBT funding.
	err := checkPsbtFlags(req)
	if err != nil {
		return err
	}

	// Generate a new, random pending channel ID that we'll use as the main
	// identifier when sending update messages to the RPC server.
	if _, err := rand.Read(pendingChanID[:]); err != nil {
		return fmt.Errorf("unable to generate random chan ID: %w", err)
	}
	fmt.Printf("Starting PSBT funding flow with pending channel ID %x.\n",
		pendingChanID)

	// maybeCancelShim is a helper function that cancels the funding shim
	// with the RPC server in case we end up aborting early.
	maybeCancelShim := func() {
		// If the user canceled while there was still a shim registered
		// with the wallet, release the resources now.
		if shimPending {
			fmt.Printf("Canceling PSBT funding flow for pending "+
				"channel ID %x.\n", pendingChanID)
			cancelMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_ShimCancel{
					ShimCancel: &lnrpc.FundingShimCancel{
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err := m.cfg.LightningClient.FundingStateStep(
				ctxc, cancelMsg,
			)
			if err != nil {
				fmt.Printf("Error canceling shim: %v\n", err)
			}
			shimPending = false
		}

		// Abort the stream connection to the server.
		cancel()
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
	rawCtx, _, rawClient := m.cfg.LightningClient.RawClientWithMacAuth(ctxc)
	stream, err := rawClient.OpenChannel(rawCtx, req)
	if err != nil {
		return fmt.Errorf("opening stream to server failed: %w", err)
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

	// Spawn another goroutine that only handles abort from user or errors
	// from the server. Both will trigger an attempt to cancel the shim with
	// the server.
	go func() {
		select {
		case <-ctx.Done():
			fmt.Printf("\nInterrupt signal received.\n")
			close(quit)

		case err := <-srvErr:
			fmt.Printf("\nError received: %v\n", err)

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

	// Our main event loop where we wait for triggers
	for {
		var srvResponse *lnrpc.OpenStatusUpdate
		select {
		case srvResponse = <-srvMsg:
		case <-quit:
			return nil
		}

		switch update := srvResponse.Update.(type) {
		case *lnrpc.OpenStatusUpdate_PsbtFund:
			amt := btcutil.Amount(update.PsbtFund.FundingAmount)
			addr := update.PsbtFund.FundingAddress
			log.Infof("PSBT funding initiated with peer %x, "+
				"funding amount %v, funding address %v",
				req.NodePubkey, amt, addr)

			// Create the psbt funding transaction for the channel.
			// Ensure the selected deposits amount to the psbt
			// funding amount.
			address, err := btcutil.DecodeAddress(
				addr, m.cfg.ChainParams,
			)
			if err != nil {
				return fmt.Errorf("decoding funding "+
					"address: %w", err)
			}

			addrParams, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
			if err != nil {
				return err
			}

			staticAddress, err := m.cfg.AddressManager.GetStaticAddress(ctx)
			if err != nil {
				return err
			}

			tx, psbtBytes, err := m.createFundingPsbt(
				ctx, deposits, amt, address, addrParams.PkScript,
			)

			verifyMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_PsbtVerify{
					PsbtVerify: &lnrpc.FundingPsbtVerify{
						FundedPsbt:    psbtBytes,
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err = m.cfg.LightningClient.FundingStateStep(
				ctxc, verifyMsg,
			)
			if err != nil {
				return fmt.Errorf("verifying PSBT by lnd "+
					"failed: %v", err)
			}

			// Now that we know the PSBT looks good we can request
			// a cooperative signature from the server.
			// Create a musig2 session for each deposit.
			sessions, clientNonces, idx, err := staticutil.CreateMusig2SessionsPerDeposit(
				ctx, m.cfg.Signer, deposits, addrParams, staticAddress,
			)
			if err != nil {
				return err
			}

			prevOuts, err := staticutil.ToPrevOuts(deposits, addrParams.PkScript)
			if err != nil {
				return err
			}

			sigReq := &serverrpc.SignOpenChannelPsbtRequest{
				OpenChannelTxPsbt: psbtBytes,
				DepositToNonces:   clientNonces,
				PrevoutInfo:       staticutil.GetPrevoutInfo(prevOuts),
			}

			sigResp, err := m.cfg.Server.SignOpenChannelPsbt(
				ctx, sigReq,
			)
			if err != nil {
				return fmt.Errorf("unable to sign open "+
					"channel psbt with the server: %w", err)
			}

			// Do some sanity checks.
			txHash := tx.TxHash()
			if !bytes.Equal(txHash.CloneBytes(), sigResp.Txid) {
				return errors.New("txid doesn't match")
			}

			if len(sigResp.SigningInfo) != len(req.Outpoints) {
				return errors.New("invalid number of " +
					"deposit signatures")
			}

			prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)

			sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)

			// Create our digest.
			var sigHash [32]byte

			// We'll now add the nonce to our session and sign the tx.
			for deposit, sigAndNonce := range sigResp.SigningInfo {
				session, ok := sessions[deposit]
				if !ok {
					return errors.New("session not found")
				}

				nonce := [musig2.PubNonceSize]byte{}
				copy(nonce[:], sigAndNonce.Nonce)
				haveAllNonces, err := m.cfg.Signer.MuSig2RegisterNonces(
					ctx, session.SessionID,
					[][musig2.PubNonceSize]byte{nonce},
				)
				if err != nil {
					return err
				}

				if !haveAllNonces {
					return errors.New("expected all " +
						"nonces to be registered")
				}

				taprootSigHash, err := txscript.CalcTaprootSignatureHash(
					sigHashes, txscript.SigHashDefault, tx,
					idx[deposit], prevOutFetcher,
				)
				if err != nil {
					return err
				}

				copy(sigHash[:], taprootSigHash)

				// Sign the tx.
				_, err = m.cfg.Signer.MuSig2Sign(
					ctx, session.SessionID, sigHash, false,
				)
				if err != nil {
					return err
				}

				// Combine the signature with the client signature.
				haveAllSigs, sig, err := m.cfg.Signer.MuSig2CombineSig(
					ctx, session.SessionID,
					[][]byte{sigAndNonce.Sig},
				)
				if err != nil {
					return err
				}

				if !haveAllSigs {
					return errors.New("expected all " +
						"signatures to be combined")
				}

				tx.TxIn[idx[deposit]].Witness = wire.TxWitness{sig} // nolint: lll
			}

			var buffer bytes.Buffer
			tx.Serialize(&buffer)
			transitionMsg := &lnrpc.FundingTransitionMsg{
				Trigger: &lnrpc.FundingTransitionMsg_PsbtFinalize{
					PsbtFinalize: &lnrpc.FundingPsbtFinalize{
						FinalRawTx:    buffer.Bytes(),
						PendingChanId: pendingChanID[:],
					},
				},
			}
			_, err = m.cfg.LightningClient.FundingStateStep(ctxc, transitionMsg)
			if err != nil {
				return fmt.Errorf("finalizing PSBT funding "+
					"flow failed: %v", err)
			}

		case *lnrpc.OpenStatusUpdate_ChanPending:
			// As soon as the channel is pending, there is no more
			// shim that needs to be canceled. If the user
			// interrupts now, we don't need to clean up anything.
			shimPending = false

		case *lnrpc.OpenStatusUpdate_ChanOpen:
			return errors.New("channel open, funding complete")
		}
	}
}

// createFundingPsbt ...
func (m *Manager) createFundingPsbt(ctx context.Context,
	deposits []*deposit.Deposit, fundingAmount btcutil.Amount,
	fundingAddress btcutil.Address, staticAddressPkScript []byte) (
	*wire.MsgTx, []byte, error) {

	msgTx := wire.NewMsgTx(2)

	// Add the deposit inputs to the transaction in the order for the server
	// signed them.
	outpoints := make([]wire.OutPoint, 0, len(deposits))
	for _, d := range deposits {
		outpoints = append(outpoints, d.OutPoint)
	}

	for _, outpoint := range outpoints {
		msgTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: outpoint,
			Witness:          [][]byte{},
		})
	}

	// Check that the sum of deposit outpoints is equal to the swap amount.
	fundingPkScript, err := txscript.PayToAddrScript(fundingAddress)
	if err != nil {
		return nil, nil, err
	}

	// Create the sweep output.
	fundingOutput := &wire.TxOut{
		Value:    int64(fundingAmount),
		PkScript: fundingPkScript,
	}

	msgTx.AddTxOut(fundingOutput)

	psbtx, err := psbt.NewFromUnsignedTx(msgTx)
	if err != nil {
		return nil, nil, err
	}
	psbtx.Inputs = []psbt.PInput{
		{WitnessUtxo: &wire.TxOut{
			Value:    int64(250_000),
			PkScript: staticAddressPkScript,
		}},
	}

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
	req *looprpc.OpenChannelRequest) (string, string, error) {

	request := newOpenChannelRequest{
		request:  req,
		respChan: make(chan *newOpenChannelResponse),
	}

	// Send the new loop-in request to the manager run loop.
	select {
	case m.newOpenChannelRequestChan <- request:

	case <-m.exitChan:
		return "", "", fmt.Errorf("open channel manager has been " +
			"canceled")

	case <-ctx.Done():
		return "", "", fmt.Errorf("context canceled while opening " +
			"channel")
	}

	// Wait for the response from the manager run loop.
	select {
	case resp := <-request.respChan:
		return "", "", resp.err

	case <-m.exitChan:
		return "", "", fmt.Errorf("open channel manager has been " +
			"canceled")

	case <-ctx.Done():
		return "", "", fmt.Errorf("context canceled while waiting " +
			"for open channel response")
	}
}
