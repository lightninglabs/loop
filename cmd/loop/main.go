package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"

	"github.com/btcsuite/btcutil"

	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

var (
	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)
)

func printRespJSON(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
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
	}
	app.Commands = []cli.Command{
		loopOutCommand, loopInCommand, termsCommand,
		monitorCommand, quoteCommand,
	}

	err := app.Run(os.Args)
	if err != nil {
		fatal(err)
	}
}

func getClient(ctx *cli.Context) (looprpc.SwapClientClient, func(), error) {
	rpcServer := ctx.GlobalString("rpcserver")
	conn, err := getClientConn(rpcServer)
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

type limits struct {
	maxSwapRoutingFee   *btcutil.Amount
	maxPrepayRoutingFee *btcutil.Amount
	maxMinerFee         btcutil.Amount
	maxSwapFee          btcutil.Amount
	maxPrepayAmt        *btcutil.Amount
}

func getLimits(amt btcutil.Amount, quote *looprpc.QuoteResponse) *limits {
	maxSwapRoutingFee := getMaxRoutingFee(amt)
	maxPrepayRoutingFee := getMaxRoutingFee(btcutil.Amount(
		quote.PrepayAmt,
	))
	maxPrepayAmt := btcutil.Amount(quote.PrepayAmt)

	return &limits{
		maxSwapRoutingFee:   &maxSwapRoutingFee,
		maxPrepayRoutingFee: &maxPrepayRoutingFee,

		// Apply a multiplier to the estimated miner fee, to not get
		// the swap canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.MinerFee) * 3,

		maxSwapFee:   btcutil.Amount(quote.SwapFee),
		maxPrepayAmt: &maxPrepayAmt,
	}
}

func displayLimits(swapType loop.Type, amt btcutil.Amount, l *limits,
	externalHtlc bool) error {

	totalSuccessMax := l.maxMinerFee + l.maxSwapFee
	if l.maxSwapRoutingFee != nil {
		totalSuccessMax += *l.maxSwapRoutingFee
	}
	if l.maxPrepayRoutingFee != nil {
		totalSuccessMax += *l.maxPrepayRoutingFee
	}

	if swapType == loop.TypeIn && externalHtlc {
		fmt.Printf("On-chain fee for external loop in is not " +
			"included.\nSufficient fees will need to be paid " +
			"when constructing the transaction in the external " +
			"wallet.\n\n")
	}

	fmt.Printf("Max swap fees for %d Loop %v: %d\n",
		amt, swapType, totalSuccessMax,
	)

	fmt.Printf("CONTINUE SWAP? (y/n), expand fee detail (x): ")

	var answer string
	fmt.Scanln(&answer)

	switch answer {
	case "y":
		return nil
	case "x":
		fmt.Println()
		if swapType != loop.TypeIn || !externalHtlc {
			fmt.Printf("Max on-chain fee:                 %d\n",
				l.maxMinerFee)
		}

		if l.maxSwapRoutingFee != nil {
			fmt.Printf("Max off-chain swap routing fee:   %d\n",
				*l.maxSwapRoutingFee)
		}

		if l.maxPrepayRoutingFee != nil {
			fmt.Printf("Max off-chain prepay routing fee: %d\n",
				*l.maxPrepayRoutingFee)
		}
		fmt.Printf("Max swap fee:                     %d\n", l.maxSwapFee)

		if l.maxPrepayAmt != nil {
			fmt.Printf("Max no show penalty:              %d\n",
				*l.maxPrepayAmt)
		}

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
	fmt.Printf("%v %v %v %v - %v",
		time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
		swap.Type, swap.State, btcutil.Amount(swap.Amt),
		swap.HtlcAddress,
	)

	if swap.State != looprpc.SwapState_INITIATED &&
		swap.State != looprpc.SwapState_HTLC_PUBLISHED &&
		swap.State != looprpc.SwapState_PREIMAGE_REVEALED {

		fmt.Printf(" (cost: server %v, onchain %v, offchain %v)",
			swap.CostServer, swap.CostOnchain, swap.CostOffchain,
		)
	}

	fmt.Println()
}

func getClientConn(address string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
