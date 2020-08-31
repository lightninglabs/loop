package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopd"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/protobuf-hex-display/json"
	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightninglabs/protobuf-hex-display/proto"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/macaroons"

	"github.com/btcsuite/btcutil"

	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

var (
	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)

	defaultSwapWaitTime = 30 * time.Minute

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// defaultMacaroonTimeout is the default macaroon timeout in seconds
	// that we set when sending it over the line.
	defaultMacaroonTimeout int64 = 60

	loopDirFlag = cli.StringFlag{
		Name:  "loopdir",
		Value: loopd.LoopDirBase,
		Usage: "path to loop's base directory",
	}
	networkFlag = cli.StringFlag{
		Name: "network, n",
		Usage: "the network loop is running on e.g. mainnet, " +
			"testnet, etc.",
		Value: loopd.DefaultNetwork,
	}

	tlsCertFlag = cli.StringFlag{
		Name: "tlscertpath",
		Usage: "path to loop's TLS certificate, only needed if loop " +
			"runs in the same process as lnd",
		Value: loopd.DefaultTLSCertPath,
	}
	macaroonPathFlag = cli.StringFlag{
		Name: "macaroonpath",
		Usage: "path to macaroon file, only needed if loop runs " +
			"in the same process as lnd",
	}
	noMacaroonsFlag = cli.BoolFlag{
		Name:  "no-macaroons",
		Usage: "disable macaroon authentication",
	}
)

func printJSON(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	err = json.Indent(&out, b, "", "\t")
	if err != nil {
		fatal(err)
	}
	out.WriteString("\n")
	_, _ = out.WriteTo(os.Stdout)
}

func printRespJSON(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
		OrigName:     true,
		EmitDefaults: true,
		Indent:       "    ",
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[loop] %v\n", err)
	os.Exit(1)
}

func main() {
	app := cli.NewApp()

	app.Version = loop.Version()
	app.Name = "loop"
	app.Usage = "control plane for your loopd"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:11010",
			Usage: "loopd daemon address host:port",
		},
		networkFlag,
		loopDirFlag,
		tlsCertFlag,
		macaroonPathFlag,
		noMacaroonsFlag,
	}
	app.Commands = []cli.Command{
		loopOutCommand, loopInCommand, termsCommand,
		monitorCommand, quoteCommand, listAuthCommand,
		listSwapsCommand, swapInfoCommand, bakeMacaroonCommand,
		listPermissionsCommand,
	}

	err := app.Run(os.Args)
	if err != nil {
		fatal(err)
	}
}

func getClient(ctx *cli.Context) (looprpc.SwapClientClient, func(), error) {
	rpcServer := ctx.GlobalString("rpcserver")
	tlsCertPath, macaroonPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	conn, err := getClientConn(rpcServer, tlsCertPath, macaroonPath)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	loopClient := looprpc.NewSwapClientClient(conn)
	return loopClient, cleanup, nil
}

func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}

// extractPathArgs parses the TLS certificate and macaroon paths from the
// command.
func extractPathArgs(ctx *cli.Context) (string, string, error) {
	// We'll start off by parsing the network. This is needed to determine
	// the correct path to the TLS certificate and macaroon when not
	// specified.
	network := strings.ToLower(ctx.GlobalString("network"))
	switch network {
	case "mainnet", "testnet", "regtest", "simnet":
	default:
		return "", "", fmt.Errorf("unknown network: %v", network)
	}

	// We'll now fetch the loopdir so we can make a decision on how to
	// properly read the macaroons (if needed) and also the cert. This will
	// either be the default, or will have been overwritten by the end
	// user.
	loopDir := lncfg.CleanAndExpandPath(ctx.GlobalString(loopDirFlag.Name))

	// If the macaroon path as been manually provided, then we'll only
	// target the specified file.
	var macPath string
	switch {
	case ctx.GlobalBool(noMacaroonsFlag.Name):
		macPath = ""

	case ctx.GlobalIsSet(macaroonPathFlag.Name):
		macPath = lncfg.CleanAndExpandPath(ctx.GlobalString(
			macaroonPathFlag.Name,
		))

	default:
		// Otherwise, we'll go into the path:
		// loopdir/<network> in order to fetch the macaroon that we
		// need.
		macPath = filepath.Join(
			loopDir, network, loopd.DefaultMacFilename,
		)
	}

	tlsCertPath := lncfg.CleanAndExpandPath(ctx.GlobalString(
		tlsCertFlag.Name,
	))

	// If a custom lnd directory was set, we'll also check if custom paths
	// for the TLS cert and macaroon file were set as well. If not, we'll
	// override their paths so they can be found within the custom lnd
	// directory set. This allows us to set a custom lnd directory, along
	// with custom paths to the TLS cert and macaroon file.
	if loopDir != loopd.LoopDirBase || network != loopd.DefaultNetwork {
		tlsCertPath = filepath.Join(
			loopDir, network, loopd.DefaultTLSCertFilename,
		)
	}

	return tlsCertPath, macPath, nil
}

type inLimits struct {
	maxMinerFee btcutil.Amount
	maxSwapFee  btcutil.Amount
}

func getInLimits(quote *looprpc.InQuoteResponse) *inLimits {
	return &inLimits{
		// Apply a multiplier to the estimated miner fee, to not get
		// the swap canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.HtlcPublishFeeSat) * 3,
		maxSwapFee:  btcutil.Amount(quote.SwapFeeSat),
	}
}

type outLimits struct {
	maxSwapRoutingFee   btcutil.Amount
	maxPrepayRoutingFee btcutil.Amount
	maxMinerFee         btcutil.Amount
	maxSwapFee          btcutil.Amount
	maxPrepayAmt        btcutil.Amount
}

func getOutLimits(amt btcutil.Amount,
	quote *looprpc.OutQuoteResponse) *outLimits {

	maxSwapRoutingFee := getMaxRoutingFee(amt)
	maxPrepayRoutingFee := getMaxRoutingFee(btcutil.Amount(
		quote.PrepayAmtSat,
	))
	maxPrepayAmt := btcutil.Amount(quote.PrepayAmtSat)

	return &outLimits{
		maxSwapRoutingFee:   maxSwapRoutingFee,
		maxPrepayRoutingFee: maxPrepayRoutingFee,

		// Apply a multiplier to the estimated miner fee, to not get
		// the swap canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.HtlcSweepFeeSat) * 100,

		maxSwapFee:   btcutil.Amount(quote.SwapFeeSat),
		maxPrepayAmt: maxPrepayAmt,
	}
}

func displayInLimits(amt, minerFees btcutil.Amount, l *inLimits,
	externalHtlc bool) error {

	totalSuccessMax := l.maxMinerFee + l.maxSwapFee

	if externalHtlc {
		fmt.Printf("On-chain fee for external loop in is not " +
			"included.\nSufficient fees will need to be paid " +
			"when constructing the transaction in the external " +
			"wallet.\n\n")
	}

	fmt.Printf("Max swap fees for %d sat Loop %v: %d sat\n", amt, swap.TypeIn,
		totalSuccessMax)

	fmt.Printf("CONTINUE SWAP? (y/n), expand fee detail (x): ")

	var answer string
	fmt.Scanln(&answer)

	switch answer {
	case "y":
		return nil
	case "x":
		fmt.Println()
		f := "%-36s %d sat\n"

		if !externalHtlc {
			fmt.Printf(f, "Estimated on-chain HTLC fee:",
				minerFees)
		}

		fmt.Printf(f, "Max swap fee:", l.maxSwapFee)

		fmt.Printf("CONTINUE SWAP? (y/n): ")
		fmt.Scanln(&answer)
		if answer == "y" {
			return nil
		}
	}

	return errors.New("swap canceled")
}

func displayOutLimits(amt, minerFees btcutil.Amount, l *outLimits,
	warning string) error {

	totalSuccessMax := l.maxMinerFee + l.maxSwapFee + l.maxSwapRoutingFee +
		l.maxPrepayRoutingFee

	fmt.Printf("Max swap fees for %d sat Loop %v: %d sat\n", amt, swap.TypeOut,
		totalSuccessMax)

	if warning != "" {
		fmt.Println(warning)
	}

	fmt.Printf("CONTINUE SWAP? (y/n), expand fee detail (x): ")

	var answer string
	fmt.Scanln(&answer)

	switch answer {
	case "y":
		return nil
	case "x":
		fmt.Println()
		f := "%-36s %d sat\n"

		fmt.Printf(f, "Estimated on-chain sweep fee:", minerFees)
		fmt.Printf(f, "Max on-chain sweep fee:", l.maxMinerFee)
		fmt.Printf(f, "Max off-chain swap routing fee:",
			l.maxSwapRoutingFee)
		fmt.Printf(f, "Max no show penalty (prepay):", l.maxPrepayAmt)
		fmt.Printf(f, "Max off-chain prepay routing fee:",
			l.maxPrepayRoutingFee)
		fmt.Printf(f, "Max swap fee:", l.maxSwapFee)

		fmt.Printf("CONTINUE SWAP? (y/n): ")
		fmt.Scanln(&answer)
		if answer == "y" {
			return nil
		}
	}

	return errors.New("swap canceled")
}

func parseAmt(text string) (btcutil.Amount, error) {
	amtInt64, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amt value")
	}
	return btcutil.Amount(amtInt64), nil
}

func logSwap(swap *looprpc.SwapStatus) {
	// If our swap failed, we add our failure reason to the state.
	swapState := fmt.Sprintf("%v", swap.State)
	if swap.State == looprpc.SwapState_FAILED {
		swapState = fmt.Sprintf("%v (%v)", swapState, swap.FailureReason)
	}

	if swap.Type == looprpc.SwapType_LOOP_OUT {
		fmt.Printf("%v %v %v %v - %v",
			time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
			swap.Type, swapState, btcutil.Amount(swap.Amt),
			swap.HtlcAddressP2Wsh,
		)
	} else {
		fmt.Printf("%v %v %v %v -",
			time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
			swap.Type, swapState, btcutil.Amount(swap.Amt))
		if swap.HtlcAddressP2Wsh != "" {
			fmt.Printf(" P2WSH: %v", swap.HtlcAddressP2Wsh)
		}

		if swap.HtlcAddressNp2Wsh != "" {
			fmt.Printf(" NP2WSH: %v", swap.HtlcAddressNp2Wsh)
		}
	}

	if swap.State != looprpc.SwapState_INITIATED &&
		swap.State != looprpc.SwapState_HTLC_PUBLISHED &&
		swap.State != looprpc.SwapState_PREIMAGE_REVEALED {

		fmt.Printf(" (cost: server %v, onchain %v, offchain %v)",
			swap.CostServer, swap.CostOnchain, swap.CostOffchain,
		)
	}

	fmt.Println()
}

func getClientConn(address, tlsCertPath, macaroonPath string) (*grpc.ClientConn,
	error) {

	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	// TLS cannot be disabled, we'll always have a cert file to read.
	creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		fatal(err)
	}

	// Macaroons can be disabled.
	if macaroonPath != "" {
		opts = append(opts, readMacaroon(macaroonPath))
	}

	opts = append(opts, grpc.WithTransportCredentials(creds))

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

// readMacaroon tries to read the macaroon file at the specified path and create
// gRPC dial options from it.
func readMacaroon(macPath string) grpc.DialOption {
	// Load the specified macaroon file.
	macBytes, err := ioutil.ReadFile(macPath)
	if err != nil {
		fatal(fmt.Errorf("unable to read macaroon path : %v", err))
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		fatal(fmt.Errorf("unable to decode macaroon: %v", err))
	}

	macConstraints := []macaroons.Constraint{
		// We add a time-based constraint to prevent replay of the
		// macaroon. It's good for 60 seconds by default to make up for
		// any discrepancy between client and server clocks, but leaking
		// the macaroon before it becomes invalid makes it possible for
		// an attacker to reuse the macaroon. In addition, the validity
		// time of the macaroon is extended by the time the server clock
		// is behind the client clock, or shortened by the time the
		// server clock is ahead of the client clock (or invalid
		// altogether if, in the latter case, this time is more than 60
		// seconds).
		macaroons.TimeoutConstraint(defaultMacaroonTimeout),
	}

	// Apply constraints to the macaroon.
	constrainedMac, err := macaroons.AddConstraints(mac, macConstraints...)
	if err != nil {
		fatal(err)
	}

	// Now we append the macaroon credentials to the dial options.
	cred := macaroons.NewMacaroonCredential(constrainedMac)
	return grpc.WithPerRPCCredentials(cred)
}
