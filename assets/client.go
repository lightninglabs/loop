package assets

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/taproot-assets/rfqmath"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/tapcfg"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/priceoraclerpc"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	universerpc.UniverseClient

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
	conn, err := grpc.NewClient(config.Host, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}
