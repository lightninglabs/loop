package assets

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"bytes"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	tap "github.com/lightninglabs/taproot-assets"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/tapcfg"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	wrpc "github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/priceoraclerpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapdevrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightninglabs/taproot-assets/universe"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
)

var (

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// defaultRfqTimeout is the default timeout we wait for tapd peer to
	// accept RFQ.
	defaultRfqTimeout = time.Second * 60
)

// TapdConfig is a struct that holds the configuration options to connect to a
// taproot assets daemon.
type TapdConfig struct {
	Activate     bool          `long:"activate" description:"Activate the Tap daemon"`
	Host         string        `long:"host" description:"The host of the Tap daemon, in the format of host:port"`
	MacaroonPath string        `long:"macaroonpath" description:"Path to the admin macaroon"`
	TLSPath      string        `long:"tlspath" description:"Path to the TLS certificate"`
	RFQtimeout   time.Duration `long:"rfqtimeout" description:"The timeout we wait for tapd peer to accept RFQ"`
}

// DefaultTapdConfig returns a default configuration to connect to a taproot
// assets daemon.
func DefaultTapdConfig() *TapdConfig {
	defaultConf := tapcfg.DefaultConfig()
	return &TapdConfig{
		Activate:     false,
		Host:         "localhost:10029",
		MacaroonPath: defaultConf.RpcConf.MacaroonPath,
		TLSPath:      defaultConf.RpcConf.TLSCertPath,
		RFQtimeout:   defaultRfqTimeout,
	}
}

// TapdClient is a client for the Tap daemon.
type TapdClient struct {
	taprpc.TaprootAssetsClient
	tapchannelrpc.TaprootAssetChannelsClient
	priceoraclerpc.PriceOracleClient
	rfqrpc.RfqClient
	wrpc.AssetWalletClient
	mintrpc.MintClient
	universerpc.UniverseClient
	tapdevrpc.TapDevClient

	cfg            *TapdConfig
	assetNameCache map[string]string
	assetNameMutex sync.Mutex
	cc             *grpc.ClientConn
}

// NewTapdClient returns a new taproot assets client.
func NewTapdClient(config *TapdConfig) (*TapdClient, error) {
	// Create the client connection to the server.
	conn, err := getClientConn(config)
	if err != nil {
		return nil, err
	}

	// Create the TapdClient.
	client := &TapdClient{
		assetNameCache:             make(map[string]string),
		cc:                         conn,
		cfg:                        config,
		TaprootAssetsClient:        taprpc.NewTaprootAssetsClient(conn),
		TaprootAssetChannelsClient: tapchannelrpc.NewTaprootAssetChannelsClient(conn),
		PriceOracleClient:          priceoraclerpc.NewPriceOracleClient(conn),
		RfqClient:                  rfqrpc.NewRfqClient(conn),
		UniverseClient:             universerpc.NewUniverseClient(conn),
	}

	return client, nil
}

// Close closes the client connection to the server.
func (c *TapdClient) Close() {
	c.cc.Close()
}

// GetRfqForAsset returns a RFQ for the given asset with the given amount and
// to the given peer.
func (c *TapdClient) GetRfqForAsset(ctx context.Context,
	satAmount btcutil.Amount, assetId, peerPubkey []byte,
	expiry int64, feeLimitMultiplier float64) (
	*rfqrpc.PeerAcceptedSellQuote, error) {

	// paymentMaxAmt is the maximum amount we are willing to pay for the
	// payment.
	// E.g. on a 250k sats payment we'll multiply the sat amount by 1.2.
	// The resulting maximum amount we're willing to pay is 300k sats.
	// The response asset amount will be for those 300k sats.
	paymentMaxAmt, err := getPaymentMaxAmount(satAmount, feeLimitMultiplier)
	if err != nil {
		return nil, err
	}

	rfq, err := c.RfqClient.AddAssetSellOrder(
		ctx, &rfqrpc.AddAssetSellOrderRequest{
			AssetSpecifier: &rfqrpc.AssetSpecifier{
				Id: &rfqrpc.AssetSpecifier_AssetId{
					AssetId: assetId,
				},
			},
			PeerPubKey:     peerPubkey,
			PaymentMaxAmt:  uint64(paymentMaxAmt),
			Expiry:         uint64(expiry),
			TimeoutSeconds: uint32(c.cfg.RFQtimeout.Seconds()),
		})
	if err != nil {
		return nil, err
	}
	if rfq.GetInvalidQuote() != nil {
		return nil, fmt.Errorf("invalid RFQ: %v", rfq.GetInvalidQuote())
	}
	if rfq.GetRejectedQuote() != nil {
		return nil, fmt.Errorf("rejected RFQ: %v",
			rfq.GetRejectedQuote())
	}

	if rfq.GetAcceptedQuote() != nil {
		return rfq.GetAcceptedQuote(), nil
	}

	return nil, fmt.Errorf("no accepted quote")
}

// GetAssetName returns the human-readable name of the asset.
func (c *TapdClient) GetAssetName(ctx context.Context,
	assetId []byte) (string, error) {

	c.assetNameMutex.Lock()
	defer c.assetNameMutex.Unlock()
	assetIdStr := hex.EncodeToString(assetId)
	if name, ok := c.assetNameCache[assetIdStr]; ok {
		return name, nil
	}

	assetStats, err := c.UniverseClient.QueryAssetStats(
		ctx, &universerpc.AssetStatsQuery{
			AssetIdFilter: assetId,
		},
	)
	if err != nil {
		return "", err
	}

	if len(assetStats.AssetStats) == 0 {
		return "", fmt.Errorf("asset not found")
	}

	var assetName string

	// If the asset belongs to a group, return the group name.
	if assetStats.AssetStats[0].GroupAnchor != nil {
		assetName = assetStats.AssetStats[0].GroupAnchor.AssetName
	} else {
		assetName = assetStats.AssetStats[0].Asset.AssetName
	}

	c.assetNameCache[assetIdStr] = assetName

	return assetName, nil
}

// GetAssetPrice returns the price of an asset in satoshis. NOTE: this currently
// uses the rfq process for the asset price. A future implementation should
// use a price oracle to not spam a peer.
func (c *TapdClient) GetAssetPrice(ctx context.Context, assetID string,
	peerPubkey []byte, assetAmt uint64, paymentMaxAmt btcutil.Amount) (
	btcutil.Amount, error) {

	// We'll allow a short rfq expiry as we'll only use this rfq to
	// gauge a price.
	rfqExpiry := time.Now().Add(time.Minute).Unix()

	msatAmt := lnwire.NewMSatFromSatoshis(paymentMaxAmt)

	// First we'll rfq a random peer for the asset.
	rfq, err := c.RfqClient.AddAssetSellOrder(
		ctx, &rfqrpc.AddAssetSellOrderRequest{
			AssetSpecifier: &rfqrpc.AssetSpecifier{
				Id: &rfqrpc.AssetSpecifier_AssetIdStr{
					AssetIdStr: assetID,
				},
			},
			PaymentMaxAmt:  uint64(msatAmt),
			Expiry:         uint64(rfqExpiry),
			TimeoutSeconds: uint32(c.cfg.RFQtimeout.Seconds()),
			PeerPubKey:     peerPubkey,
		})
	if err != nil {
		return 0, err
	}
	if rfq == nil {
		return 0, fmt.Errorf("no RFQ response")
	}

	if rfq.GetInvalidQuote() != nil {
		return 0, fmt.Errorf("peer %v sent an invalid quote response %v for "+
			"asset %v", peerPubkey, rfq.GetInvalidQuote(), assetID)
	}

	if rfq.GetRejectedQuote() != nil {
		return 0, fmt.Errorf("peer %v rejected the quote request for "+
			"asset %v, %v", peerPubkey, assetID, rfq.GetRejectedQuote())
	}

	acceptedRes := rfq.GetAcceptedQuote()
	if acceptedRes == nil {
		return 0, fmt.Errorf("no accepted quote")
	}

	// We'll use the accepted quote to calculate the price.
	return getSatsFromAssetAmt(assetAmt, acceptedRes.BidAssetRate)
}

// getSatsFromAssetAmt returns the amount in satoshis for the given asset amount
// and asset rate.
func getSatsFromAssetAmt(assetAmt uint64, assetRate *rfqrpc.FixedPoint) (
	btcutil.Amount, error) {

	rateFP, err := rpcutils.UnmarshalRfqFixedPoint(assetRate)
	if err != nil {
		return 0, fmt.Errorf("cannot unmarshal asset rate: %w", err)
	}

	assetUnits := rfqmath.NewBigIntFixedPoint(assetAmt, 0)

	msatAmt := rfqmath.UnitsToMilliSatoshi(assetUnits, *rateFP)

	return msatAmt.ToSatoshis(), nil
}

// getPaymentMaxAmount returns the milisat amount we are willing to pay for the
// payment.
func getPaymentMaxAmount(satAmount btcutil.Amount, feeLimitMultiplier float64) (
	lnwire.MilliSatoshi, error) {

	if satAmount == 0 {
		return 0, fmt.Errorf("satAmount cannot be zero")
	}
	if feeLimitMultiplier < 1 {
		return 0, fmt.Errorf("feeLimitMultiplier must be at least 1")
	}

	// paymentMaxAmt is the maximum amount we are willing to pay for the
	// payment.
	// E.g. on a 250k sats payment we'll multiply the sat amount by 1.2.
	// The resulting maximum amount we're willing to pay is 300k sats.
	// The response asset amount will be for those 300k sats.
	return lnrpc.UnmarshallAmt(
		int64(satAmount.MulF64(feeLimitMultiplier)), 0,
	)
}

func getClientConn(config *TapdConfig) (*grpc.ClientConn, error) {
	// Load the specified TLS certificate and build transport credentials.
	creds, err := credentials.NewClientTLSFromFile(config.TLSPath, "")
	if err != nil {
		return nil, err
	}

	// Load the specified macaroon file.
	macBytes, err := os.ReadFile(config.MacaroonPath)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}

	macaroon, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, err
	}
	// Create the DialOptions with the macaroon credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroon),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	// Dial the gRPC server.
	conn, err := grpc.Dial(config.Host, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// FundAndSignVpacket funds and signs a vpacket.
func (t *TapdClient) FundAndSignVpacket(ctx context.Context,
	vpkt *tappsbt.VPacket) (*tappsbt.VPacket, error) {

	// Fund the packet.
	var buf bytes.Buffer
	err := vpkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	fundResp, err := t.FundVirtualPsbt(
		ctx, &assetwalletrpc.FundVirtualPsbtRequest{
			Template: &assetwalletrpc.FundVirtualPsbtRequest_Psbt{
				Psbt: buf.Bytes(),
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// Sign the packet.
	signResp, err := t.SignVirtualPsbt(
		ctx, &assetwalletrpc.SignVirtualPsbtRequest{
			FundedPsbt: fundResp.FundedPsbt,
		},
	)
	if err != nil {
		return nil, err
	}

	return tappsbt.NewFromRawBytes(
		bytes.NewReader(signResp.SignedPsbt), false,
	)
}

// addP2WPKHOutputToPsbt adds a normal bitcoin P2WPKH output to a psbt for the
// given key and amount.
func addP2WPKHOutputToPsbt(packet *psbt.Packet, keyDesc keychain.KeyDescriptor,
	amount btcutil.Amount, params *chaincfg.Params) error {

	derivation, _, _ := btcwallet.Bip32DerivationFromKeyDesc(
		keyDesc, params.HDCoinType,
	)

	// Convert to Bitcoin address.
	pubKeyBytes := keyDesc.PubKey.SerializeCompressed()
	pubKeyHash := btcutil.Hash160(pubKeyBytes)
	address, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, params)
	if err != nil {
		return err
	}

	// Generate the P2WPKH scriptPubKey.
	scriptPubKey, err := txscript.PayToAddrScript(address)
	if err != nil {
		return err
	}

	// Add the output to the packet.
	packet.UnsignedTx.AddTxOut(
		wire.NewTxOut(int64(amount), scriptPubKey),
	)

	packet.Outputs = append(packet.Outputs, psbt.POutput{
		Bip32Derivation: []*psbt.Bip32Derivation{
			derivation,
		},
	})

	return nil
}

// PrepareAndCommitVirtualPsbts prepares and commits virtual psbt to a BTC
// template so that the underlying wallet can fund the transaction and add
// the necessary additional input to pay for fees as well as a change output
// if the change keydescriptor is not provided.
func (t *TapdClient) PrepareAndCommitVirtualPsbts(ctx context.Context,
	vpkt *tappsbt.VPacket, feeRateSatPerVByte chainfee.SatPerVByte,
	changeKeyDesc *keychain.KeyDescriptor, params *chaincfg.Params) (
	*psbt.Packet, []*tappsbt.VPacket, []*tappsbt.VPacket,
	*assetwalletrpc.CommitVirtualPsbtsResponse, error) {

	encodedVpkt, err := tappsbt.Encode(vpkt)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	btcPkt, err := tapsend.PrepareAnchoringTemplate(
		[]*tappsbt.VPacket{vpkt},
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	commitRequest := &assetwalletrpc.CommitVirtualPsbtsRequest{
		Fees: &assetwalletrpc.CommitVirtualPsbtsRequest_SatPerVbyte{
			SatPerVbyte: uint64(feeRateSatPerVByte),
		},
		AnchorChangeOutput: &assetwalletrpc.CommitVirtualPsbtsRequest_Add{ //nolint:lll
			Add: true,
		},
		VirtualPsbts: [][]byte{
			encodedVpkt,
		},
	}
	if changeKeyDesc != nil {
		err = addP2WPKHOutputToPsbt(
			btcPkt, *changeKeyDesc, btcutil.Amount(1), params,
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		commitRequest.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_ExistingOutputIndex{ //nolint:lll
				ExistingOutputIndex: 1,
			}
	} else {
		commitRequest.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_Add{
				Add: true,
			}
	}
	var buf bytes.Buffer
	err = btcPkt.Serialize(&buf)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	commitRequest.AnchorPsbt = buf.Bytes()

	commitResponse, err := t.AssetWalletClient.CommitVirtualPsbts(
		ctx, commitRequest,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	fundedPacket, err := psbt.NewFromRawBytes(
		bytes.NewReader(commitResponse.AnchorPsbt), false,
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	activePackets := make(
		[]*tappsbt.VPacket, len(commitResponse.VirtualPsbts),
	)
	for idx := range commitResponse.VirtualPsbts {
		activePackets[idx], err = tappsbt.Decode(
			commitResponse.VirtualPsbts[idx],
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	passivePackets := make(
		[]*tappsbt.VPacket, len(commitResponse.PassiveAssetPsbts),
	)
	for idx := range commitResponse.PassiveAssetPsbts {
		passivePackets[idx], err = tappsbt.Decode(
			commitResponse.PassiveAssetPsbts[idx],
		)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	return fundedPacket, activePackets, passivePackets, commitResponse, nil
}

// LogAndPublish logs and publishes a psbt with the given active and passive
// assets.
func (t *TapdClient) LogAndPublish(ctx context.Context, btcPkt *psbt.Packet,
	activeAssets []*tappsbt.VPacket, passiveAssets []*tappsbt.VPacket,
	commitResp *assetwalletrpc.CommitVirtualPsbtsResponse) (
	*taprpc.SendAssetResponse, error) {

	var buf bytes.Buffer
	err := btcPkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	request := &assetwalletrpc.PublishAndLogRequest{
		AnchorPsbt:        buf.Bytes(),
		VirtualPsbts:      make([][]byte, len(activeAssets)),
		PassiveAssetPsbts: make([][]byte, len(passiveAssets)),
		ChangeOutputIndex: commitResp.ChangeOutputIndex,
		LndLockedUtxos:    commitResp.LndLockedUtxos,
	}

	for idx := range activeAssets {
		request.VirtualPsbts[idx], err = tappsbt.Encode(
			activeAssets[idx],
		)
		if err != nil {
			return nil, err
		}
	}
	for idx := range passiveAssets {
		request.PassiveAssetPsbts[idx], err = tappsbt.Encode(
			passiveAssets[idx],
		)
		if err != nil {
			return nil, err
		}
	}

	resp, err := t.PublishAndLogTransfer(ctx, request)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// ListAvailableAssets returns a list of available assets.
func (t *TapdClient) ListAvailableAssets(ctx context.Context) (
	[][]byte, error) {

	balanceRes, err := t.ListBalances(ctx, &taprpc.ListBalancesRequest{
		GroupBy: &taprpc.ListBalancesRequest_AssetId{
			AssetId: true,
		},
	})
	if err != nil {
		return nil, err
	}

	assets := make([][]byte, 0, len(balanceRes.AssetBalances))
	for assetID := range balanceRes.AssetBalances {
		asset, err := hex.DecodeString(assetID)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}

	return assets, nil
}

// GetAssetBalance checks the balance of an asset by its ID.
func (t *TapdClient) GetAssetBalance(ctx context.Context, assetId []byte) (
	uint64, error) {

	// Check if we have enough funds to do the swap.
	balanceResp, err := t.ListBalances(
		ctx, &taprpc.ListBalancesRequest{
			GroupBy: &taprpc.ListBalancesRequest_AssetId{
				AssetId: true,
			},
			AssetFilter: assetId,
		},
	)
	if err != nil {
		return 0, err
	}

	// Check if we have enough funds to do the swap.
	balance, ok := balanceResp.AssetBalances[hex.EncodeToString(
		assetId,
	)]
	if !ok {
		return 0, status.Error(codes.Internal, "internal error")
	}

	return balance.Balance, nil
}

// GetUnEncumberedAssetBalance returns the total balance of the given asset for
// which the given client owns the script keys.
func (t *TapdClient) GetUnEncumberedAssetBalance(ctx context.Context,
	assetID []byte) (uint64, error) {

	allAssets, err := t.ListAssets(ctx, &taprpc.ListAssetRequest{})
	if err != nil {
		return 0, err
	}

	var balance uint64
	for _, a := range allAssets.Assets {
		// Only count assets from the given asset ID.
		if !bytes.Equal(a.AssetGenesis.AssetId, assetID) {
			continue
		}

		// Non-local means we don't have the internal key to spend the
		// asset.
		if !a.ScriptKeyIsLocal {
			continue
		}

		// If the asset is not declared known or has a script path, we
		// can't spend it directly.
		if !a.ScriptKeyDeclaredKnown || a.ScriptKeyHasScriptPath {
			continue
		}

		balance += a.Amount
	}

	return balance, nil
}

// DeriveNewKeys derives a new internal and script key.
func (t *TapdClient) DeriveNewKeys(ctx context.Context) (asset.ScriptKey,
	keychain.KeyDescriptor, error) {

	scriptKeyDesc, err := t.NextScriptKey(
		ctx, &assetwalletrpc.NextScriptKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	scriptKey, err := rpcutils.UnmarshalScriptKey(scriptKeyDesc.ScriptKey)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	internalKeyDesc, err := t.NextInternalKey(
		ctx, &assetwalletrpc.NextInternalKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}
	internalKeyLnd, err := rpcutils.UnmarshalKeyDescriptor(
		internalKeyDesc.InternalKey,
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	return *scriptKey, internalKeyLnd, nil
}

// ImportProofFile imports the proof file and returns the last proof.
func (t *TapdClient) ImportProofFile(ctx context.Context, rawProofFile []byte) (
	*proof.Proof, error) {

	proofFile, err := proof.DecodeFile(rawProofFile)
	if err != nil {
		return nil, err
	}

	var lastProof *proof.Proof

	for i := 0; i < proofFile.NumProofs(); i++ {
		lastProof, err = proofFile.ProofAt(uint32(i))
		if err != nil {
			return nil, err
		}

		var proofBytes bytes.Buffer
		err = lastProof.Encode(&proofBytes)
		if err != nil {
			return nil, err
		}

		asset := lastProof.Asset

		proofType := universe.ProofTypeTransfer
		if asset.IsGenesisAsset() {
			proofType = universe.ProofTypeIssuance
		}

		uniID := universe.Identifier{
			AssetID:   asset.ID(),
			ProofType: proofType,
		}
		if asset.GroupKey != nil {
			uniID.GroupKey = &asset.GroupKey.GroupPubKey
		}

		rpcUniID, err := tap.MarshalUniID(uniID)
		if err != nil {
			return nil, err
		}

		outpoint := &universerpc.Outpoint{
			HashStr: lastProof.AnchorTx.TxHash().String(),
			Index:   int32(lastProof.InclusionProof.OutputIndex),
		}

		scriptKey := lastProof.Asset.ScriptKey.PubKey
		leafKey := &universerpc.AssetKey{
			Outpoint: &universerpc.AssetKey_Op{
				Op: outpoint,
			},
			ScriptKey: &universerpc.AssetKey_ScriptKeyBytes{
				ScriptKeyBytes: scriptKey.SerializeCompressed(),
			},
		}

		_, err = t.InsertProof(ctx, &universerpc.AssetProof{
			Key: &universerpc.UniverseKey{
				Id:      rpcUniID,
				LeafKey: leafKey,
			},
			AssetLeaf: &universerpc.AssetLeaf{
				Proof: proofBytes.Bytes(),
			},
		})
		if err != nil {
			return nil, err
		}
	}

	return lastProof, nil
}

func (t *TapdClient) AddHoldInvoice(ctx context.Context, pHash lntypes.Hash,
	assetId []byte, assetAmt uint64, memo string) (
	*tapchannelrpc.AddInvoiceResponse, error) {

	// Now we can create the swap invoice.
	invoiceRes, err := t.AddInvoice(
		ctx, &tapchannelrpc.AddInvoiceRequest{

			// Todo(sputn1ck):if we have more than one peer, we'll need to
			// specify one. This will likely be changed on the tapd front in
			// the future.
			PeerPubkey:  nil,
			AssetId:     assetId,
			AssetAmount: assetAmt,
			InvoiceRequest: &lnrpc.Invoice{
				Memo:  memo,
				RHash: pHash[:],
				// todo fix expiries
				CltvExpiry: 144,
				Expiry:     60,
				Private:    true,
			},
			HodlInvoice: &tapchannelrpc.HodlInvoice{
				PaymentHash: pHash[:],
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return invoiceRes, nil
}

func (t *TapdClient) SendPayment(ctx context.Context,
	invoice string, assetId []byte) (chan *tapchannelrpc.SendPaymentResponse,
	chan error, error) {

	req := &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice,
	}

	sendReq := &tapchannelrpc.SendPaymentRequest{
		AssetId:        assetId,
		PaymentRequest: req,
	}

	sendResp, err := t.TaprootAssetChannelsClient.SendPayment(
		ctx, sendReq,
	)
	if err != nil {
		return nil, nil, err
	}

	sendRespChan := make(chan *tapchannelrpc.SendPaymentResponse)
	errChan := make(chan error)
	go func() {
		defer close(sendRespChan)
		defer close(errChan)
		for {
			select {
			case <-ctx.Done():
				errChan <- ctx.Err()
				return
			default:
				res, err := sendResp.Recv()
				if err != nil {
					errChan <- err
					return
				}
				sendRespChan <- res
			}
		}
	}()

	return sendRespChan, errChan, nil
}
