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
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/tapcfg"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	wrpc "github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/priceoraclerpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapdevrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
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

// FundAndSignVpacket funds ands signs a vpacket.
func (t *TapdClient) FundAndSignVpacket(ctx context.Context,
	vpkt *tappsbt.VPacket) (*tappsbt.VPacket, error) {

	// Fund the packet.
	var buf bytes.Buffer
	err := vpkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	fundResp, err := t.FundVirtualPsbt(
		ctx, &wrpc.FundVirtualPsbtRequest{
			Template: &wrpc.FundVirtualPsbtRequest_Psbt{
				Psbt: buf.Bytes(),
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// Sign the packet.
	signResp, err := t.SignVirtualPsbt(
		ctx, &wrpc.SignVirtualPsbtRequest{
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

// PrepareAndCommitVirtualPsbts prepares and commits virtual psbts.
func (t *TapdClient) PrepareAndCommitVirtualPsbts(ctx context.Context,
	vpkt *tappsbt.VPacket, feeRateSatPerKVByte chainfee.SatPerVByte) (
	*psbt.Packet, []*tappsbt.VPacket, []*tappsbt.VPacket,
	*wrpc.CommitVirtualPsbtsResponse, error) {

	htlcVPackets, err := tappsbt.Encode(vpkt)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	htlcBtcPkt, err := tapsend.PrepareAnchoringTemplate(
		[]*tappsbt.VPacket{vpkt},
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var buf bytes.Buffer
	err = htlcBtcPkt.Serialize(&buf)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	commitResponse, err := t.AssetWalletClient.CommitVirtualPsbts(
		ctx, &wrpc.CommitVirtualPsbtsRequest{
			AnchorPsbt: buf.Bytes(),
			Fees: &wrpc.CommitVirtualPsbtsRequest_SatPerVbyte{
				SatPerVbyte: uint64(feeRateSatPerKVByte),
			},
			AnchorChangeOutput: &wrpc.CommitVirtualPsbtsRequest_Add{
				Add: true,
			},
			VirtualPsbts: [][]byte{
				htlcVPackets,
			},
		},
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

// LogAndPublish logs and publishes the virtual psbts.
func (t *TapdClient) LogAndPublish(ctx context.Context, btcPkt *psbt.Packet,
	activeAssets []*tappsbt.VPacket, passiveAssets []*tappsbt.VPacket,
	commitResp *wrpc.CommitVirtualPsbtsResponse) (*taprpc.SendAssetResponse,
	error) {

	var buf bytes.Buffer
	err := btcPkt.Serialize(&buf)
	if err != nil {
		return nil, err
	}

	request := &wrpc.PublishAndLogRequest{
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

// CheckBalanceById checks the balance of an asset by its id.
func (t *TapdClient) CheckBalanceById(ctx context.Context, assetId []byte,
	requestedBalance btcutil.Amount) error {

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
		return err
	}

	// Check if we have enough funds to do the swap.
	balance, ok := balanceResp.AssetBalances[hex.EncodeToString(
		assetId,
	)]
	if !ok {
		return status.Error(
			codes.Internal, "internal error",
		)
	}
	if balance.Balance < uint64(requestedBalance) {
		return status.Error(
			codes.Internal, "internal error",
		)
	}

	return nil
}

// DeriveNewKeys derives a new internal and script key.
func (t *TapdClient) DeriveNewKeys(ctx context.Context) (asset.ScriptKey,
	keychain.KeyDescriptor, error) {
	scriptKeyDesc, err := t.NextScriptKey(
		ctx, &wrpc.NextScriptKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	scriptKey, err := taprpc.UnmarshalScriptKey(scriptKeyDesc.ScriptKey)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	internalKeyDesc, err := t.NextInternalKey(
		ctx, &wrpc.NextInternalKeyRequest{
			KeyFamily: uint32(asset.TaprootAssetsKeyFamily),
		},
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}
	internalKeyLnd, err := taprpc.UnmarshalKeyDescriptor(
		internalKeyDesc.InternalKey,
	)
	if err != nil {
		return asset.ScriptKey{}, keychain.KeyDescriptor{}, err
	}

	return *scriptKey, internalKeyLnd, nil
}
