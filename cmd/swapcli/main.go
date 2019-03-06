package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lightninglabs/nautilus/utils"

	"github.com/btcsuite/btcutil"

	"github.com/lightninglabs/nautilus/cmd/swapd/rpc"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

var (
	swapdAddress = "localhost:11010"

	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is the
	// minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)
	maxRoutingFeeRate = int64(50000)
)

var unchargeCommand = cli.Command{
	Name:      "uncharge",
	Usage:     "perform an off-chain to on-chain swap",
	ArgsUsage: "amt [addr]",
	Description: `
		Send the amount in satoshis specified by the amt argument on-chain.
	
		Optionally a BASE58 encoded bitcoin destination address may be 
		specified. If not specified, a new wallet address will be generated.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "channel",
			Usage: "the 8-byte compact channel ID of the channel to uncharge",
		},
	},
	Action: uncharge,
}

var termsCommand = cli.Command{
	Name:   "terms",
	Usage:  "show current server swap terms",
	Action: terms,
}

func main() {
	app := cli.NewApp()

	app.Version = "0.0.1"
	app.Usage = "command line interface to swapd"
	app.Commands = []cli.Command{unchargeCommand, termsCommand}
	app.Action = monitor

	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(err)
	}
}

func terms(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	terms, err := client.GetUnchargeTerms(
		context.Background(), &rpc.TermsRequest{},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Amount: %d - %d\n",
		btcutil.Amount(terms.MinSwapAmount),
		btcutil.Amount(terms.MaxSwapAmount),
	)
	if err != nil {
		return err
	}

	printTerms := func(terms *rpc.TermsResponse) {
		fmt.Printf("Amount: %d - %d\n",
			btcutil.Amount(terms.MinSwapAmount),
			btcutil.Amount(terms.MaxSwapAmount),
		)
		fmt.Printf("Fee:    %d + %.4f %% (%d prepaid)\n",
			btcutil.Amount(terms.SwapFeeBase),
			utils.FeeRateAsPercentage(terms.SwapFeeRate),
			btcutil.Amount(terms.PrepayAmt),
		)

		fmt.Printf("Cltv delta:   %v blocks\n", terms.CltvDelta)
	}

	fmt.Println("Uncharge")
	fmt.Println("--------")
	printTerms(terms)

	return nil
}

func monitor(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	stream, err := client.Monitor(
		context.Background(), &rpc.MonitorRequest{})
	if err != nil {
		return err
	}

	for {
		swap, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv: %v", err)
		}
		logSwap(swap)
	}
}

func getClient(ctx *cli.Context) (rpc.SwapClientClient, func(), error) {
	conn, err := getSwapCliConn(swapdAddress)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	swapCliClient := rpc.NewSwapClientClient(conn)
	return swapCliClient, cleanup, nil
}

func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return utils.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}

type limits struct {
	maxSwapRoutingFee   btcutil.Amount
	maxPrepayRoutingFee btcutil.Amount
	maxMinerFee         btcutil.Amount
	maxSwapFee          btcutil.Amount
	maxPrepayAmt        btcutil.Amount
}

func getLimits(amt btcutil.Amount, quote *rpc.QuoteResponse) *limits {
	return &limits{
		maxSwapRoutingFee: getMaxRoutingFee(btcutil.Amount(amt)),
		maxPrepayRoutingFee: getMaxRoutingFee(btcutil.Amount(
			quote.PrepayAmt,
		)),

		// Apply a multiplier to the estimated miner fee, to not get the swap
		// canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.MinerFee) * 3,

		maxSwapFee:   btcutil.Amount(quote.SwapFee),
		maxPrepayAmt: btcutil.Amount(quote.PrepayAmt),
	}
}

func displayLimits(amt btcutil.Amount, l *limits) error {
	totalSuccessMax := l.maxSwapRoutingFee + l.maxPrepayRoutingFee +
		l.maxMinerFee + l.maxSwapFee

	fmt.Printf("Max swap fees for %d uncharge: %d\n",
		btcutil.Amount(amt), totalSuccessMax,
	)
	fmt.Printf("CONTINUE SWAP? (y/n), expand fee detail (x): ")
	var answer string
	fmt.Scanln(&answer)
	switch answer {
	case "y":
		return nil
	case "x":
		fmt.Println()
		fmt.Printf("Max on-chain fee:                 %d\n", l.maxMinerFee)
		fmt.Printf("Max off-chain swap routing fee:   %d\n",
			l.maxSwapRoutingFee)
		fmt.Printf("Max off-chain prepay routing fee: %d\n",
			l.maxPrepayRoutingFee)
		fmt.Printf("Max swap fee:                     %d\n", l.maxSwapFee)
		fmt.Printf("Max no show penalty:              %d\n",
			l.maxPrepayAmt)

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

func uncharge(ctx *cli.Context) error {
	// Show command help if no arguments and flags were provided.
	if ctx.NArg() < 1 {
		cli.ShowCommandHelp(ctx, "uncharge")
		return nil
	}

	args := ctx.Args()

	amt, err := parseAmt(args[0])
	if err != nil {
		return err
	}

	var destAddr string
	args = args.Tail()
	if args.Present() {
		destAddr = args.First()
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	quote, err := client.GetUnchargeQuote(
		context.Background(),
		&rpc.QuoteRequest{
			Amt: int64(amt),
		},
	)
	if err != nil {
		return err
	}

	limits := getLimits(amt, quote)

	if err := displayLimits(amt, limits); err != nil {
		return err
	}

	var unchargeChannel uint64
	if ctx.IsSet("channel") {
		unchargeChannel = ctx.Uint64("channel")
	}

	resp, err := client.Uncharge(context.Background(), &rpc.UnchargeRequest{
		Amt:                 int64(amt),
		Dest:                destAddr,
		MaxMinerFee:         int64(limits.maxMinerFee),
		MaxPrepayAmt:        int64(limits.maxPrepayAmt),
		MaxSwapFee:          int64(limits.maxSwapFee),
		MaxPrepayRoutingFee: int64(limits.maxPrepayRoutingFee),
		MaxSwapRoutingFee:   int64(limits.maxSwapRoutingFee),
		UnchargeChannel:     unchargeChannel,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Swap initiated with id: %v\n", resp.Id[:8])
	fmt.Printf("Run swapcli without a command to monitor progress.\n")

	return nil
}

func logSwap(swap *rpc.SwapStatus) {
	fmt.Printf("%v %v %v %v - %v\n",
		time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
		swap.Type, swap.State, btcutil.Amount(swap.Amt),
		swap.HtlcAddress,
	)
}

func getSwapCliConn(address string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
