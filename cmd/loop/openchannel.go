package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

const (
	defaultUtxoMinConf = 1
)

var (
	channelTypeTweakless     = "tweakless"
	channelTypeAnchors       = "anchors"
	channelTypeSimpleTaproot = "taproot"
)

var openChannelCommand = &cli.Command{
	Name:  "openchannel",
	Usage: "Open a channel to a an existing peer.",
	Description: `
	Attempt to open a new channel to an existing peer with the key 
	node-key.

	The channel will be initialized with local-amt satoshis locally and
	push-amt satoshis for the remote node. Note that the push-amt is
	deducted from the specified local-amt which implies that the local-amt
	must be greater than the push-amt. Also note that specifying push-amt
	means you give that amount to the remote node as part of the channel
	opening. Once the channel is open, a channelPoint (txid:vout) of the
	funding output is returned.

	If the remote peer supports the option upfront shutdown feature bit
	(query listpeers to see their supported feature bits), an address to
	enforce payout of funds on cooperative close can optionally be provided.
	Note that if you set this value, you will not be able to cooperatively
	close out to another address.

	One can also specify a short string memo to record some useful
	information about the channel using the --memo argument. This is stored
	locally only, and is purely for reference. It has no bearing on the
	channel's operation. Max allowed length is 500 characters.`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "node_key",
			Usage: "the identity public key of the target " +
				"node/peer serialized in compressed format",
		},
		&cli.IntFlag{
			Name: "local_amt",
			Usage: "the number of satoshis the wallet should " +
				"commit to the channel",
		},
		&cli.Uint64Flag{
			Name: "base_fee_msat",
			Usage: "the base fee in milli-satoshis that will " +
				"be charged for each forwarded HTLC, " +
				"regardless of payment size",
		},
		&cli.Uint64Flag{
			Name: "fee_rate_ppm",
			Usage: "the fee rate ppm (parts per million) that " +
				"will be charged proportionally based on the " +
				"value of each forwarded HTLC, the lowest " +
				"possible rate is 0 with a granularity of " +
				"0.000001 (millionths)",
		},
		&cli.IntFlag{
			Name: "push_amt",
			Usage: "the number of satoshis to give the remote " +
				"side as part of the initial commitment " +
				"state, this is equivalent to first opening " +
				"a channel and sending the remote party " +
				"funds, but done all in one step",
		},
		&cli.Int64Flag{
			Name:   "sat_per_byte",
			Usage:  "Deprecated, use sat_per_vbyte instead.",
			Hidden: true,
		},
		&cli.Int64Flag{
			Name: "sat_per_vbyte",
			Usage: "(optional) a manual fee expressed in " +
				"sat/vbyte that should be used when crafting " +
				"the transaction",
		},
		&cli.BoolFlag{
			Name: "private",
			Usage: "make the channel private, such that it won't " +
				"be announced to the greater network, and " +
				"nodes other than the two channel endpoints " +
				"must be explicitly told about it to be able " +
				"to route through it",
		},
		&cli.Int64Flag{
			Name: "min_htlc_msat",
			Usage: "(optional) the minimum value we will require " +
				"for incoming HTLCs on the channel",
		},
		&cli.Uint64Flag{
			Name: "remote_csv_delay",
			Usage: "(optional) the number of blocks we will " +
				"require our channel counterparty to wait " +
				"before accessing its funds in case of " +
				"unilateral close. If this is not set, we " +
				"will scale the value according to the " +
				"channel size",
		},
		&cli.Uint64Flag{
			Name: "max_local_csv",
			Usage: "(optional) the maximum number of blocks that " +
				"we will allow the remote peer to require we " +
				"wait before accessing our funds in the case " +
				"of a unilateral close.",
		},
		&cli.StringFlag{
			Name: "close_address",
			Usage: "(optional) an address to enforce payout of " +
				"our funds to on cooperative close. Note " +
				"that if this value is set on channel open, " +
				"you will *not* be able to cooperatively " +
				"close to a different address.",
		},
		&cli.Uint64Flag{
			Name: "remote_max_value_in_flight_msat",
			Usage: "(optional) the maximum value in msat that " +
				"can be pending within the channel at any " +
				"given time",
		},
		&cli.StringFlag{
			Name: "channel_type",
			Usage: fmt.Sprintf("(optional) the type of channel to "+
				"propose to the remote peer (%q, %q, %q)",
				channelTypeTweakless, channelTypeAnchors,
				channelTypeSimpleTaproot),
		},
		&cli.BoolFlag{
			Name: "zero_conf",
			Usage: "(optional) whether a zero-conf channel open " +
				"should be attempted.",
		},
		&cli.BoolFlag{
			Name: "scid_alias",
			Usage: "(optional) whether a scid-alias channel type" +
				" should be negotiated.",
		},
		&cli.Uint64Flag{
			Name: "remote_reserve_sats",
			Usage: "(optional) the minimum number of satoshis we " +
				"require the remote node to keep as a direct " +
				"payment. If not specified, a default of 1% " +
				"of the channel capacity will be used.",
		},
		&cli.StringFlag{
			Name: "memo",
			Usage: `(optional) a note-to-self containing some useful
				information about the channel. This is stored
				locally only, and is purely for reference. It
				has no bearing on the channel's operation. Max
				allowed length is 500 characters`,
		},
		&cli.BoolFlag{
			Name: "fundmax",
			Usage: "if set, the wallet will attempt to commit " +
				"the maximum possible local amount to the " +
				"channel. This must not be set at the same " +
				"time as local_amt",
		},
		&cli.StringSliceFlag{
			Name: "utxo",
			Usage: "a utxo specified as outpoint(tx:idx) which " +
				"will be used to fund a channel. This flag " +
				"can be repeatedly used to fund a channel " +
				"with a selection of utxos. The selected " +
				"funds can either be entirely spent by " +
				"specifying the fundmax flag or partially by " +
				"selecting a fraction of the sum of the " +
				"outpoints in local_amt",
		},
	},
	Action: openChannel,
}

func openChannel(ctx context.Context, cmd *cli.Command) error {
	var (
		args      = cmd.Args()
		remaining []string
		ctxb      = context.Background()
		err       error
	)

	client, cleanup, err := getClient(ctx, cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Show command help if no arguments provided
	if cmd.NArg() == 0 && cmd.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, cmd, "openchannel")
		return nil
	}

	// Check that only the field sat_per_vbyte or the deprecated field
	// sat_per_byte is used.
	feeRateFlag, err := checkNotBothSet(
		cmd, "sat_per_vbyte", "sat_per_byte",
	)
	if err != nil {
		return err
	}

	minConfs := defaultUtxoMinConf
	req := &looprpc.OpenChannelRequest{
		SatPerVbyte:                cmd.Uint64(feeRateFlag),
		FundMax:                    cmd.Bool("fundmax"),
		MinHtlcMsat:                cmd.Int64("min_htlc_msat"),
		RemoteCsvDelay:             uint32(cmd.Uint64("remote_csv_delay")),
		MinConfs:                   int32(minConfs),
		SpendUnconfirmed:           minConfs == 0,
		CloseAddress:               cmd.String("close_address"),
		RemoteMaxValueInFlightMsat: cmd.Uint64("remote_max_value_in_flight_msat"),
		MaxLocalCsv:                uint32(cmd.Uint64("max_local_csv")),
		ZeroConf:                   cmd.Bool("zero_conf"),
		ScidAlias:                  cmd.Bool("scid_alias"),
		RemoteChanReserveSat:       cmd.Uint64("remote_reserve_sats"),
		Memo:                       cmd.String("memo"),
	}

	switch {
	case cmd.IsSet("node_key"):
		nodePubHex, err := hex.DecodeString(cmd.String("node_key"))
		if err != nil {
			return fmt.Errorf("unable to decode node public key: "+
				"%v", err)
		}
		req.NodePubkey = nodePubHex

	case args.Present():
		nodePubHex, err := hex.DecodeString(args.First())
		if err != nil {
			return fmt.Errorf("unable to decode node public key: "+
				"%v", err)
		}
		remaining = args.Tail()
		req.NodePubkey = nodePubHex

	default:
		return fmt.Errorf("node id argument missing")
	}

	if cmd.IsSet("utxo") {
		utxos := cmd.StringSlice("utxo")

		outpoints, err := UtxosToOutpoints(utxos)
		if err != nil {
			return fmt.Errorf("unable to decode utxos: %w", err)
		}

		req.Outpoints = outpoints
	}

	// The fundmax flag is NOT allowed to be combined with local_amt above.
	// It is allowed to be combined with push_amt, but only if explicitly
	// set.
	if cmd.Bool("fundmax") && req.LocalFundingAmount != 0 {
		return fmt.Errorf("local amount cannot be set if attempting " +
			"to commit the maximum amount out of the wallet")
	}

	switch {
	case cmd.IsSet("local_amt"):
		req.LocalFundingAmount = int64(cmd.Int("local_amt"))

	case !cmd.Bool("fundmax"):
		return fmt.Errorf("either local_amt or fundmax must be " +
			"specified")
	}

	if cmd.IsSet("push_amt") {
		req.PushSat = int64(cmd.Int("push_amt"))
	} else if len(remaining) > 0 {
		req.PushSat, err = strconv.ParseInt(remaining[0], 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode push amt: %w", err)
		}
	}

	if cmd.IsSet("base_fee_msat") {
		req.BaseFee = cmd.Uint64("base_fee_msat")
		req.UseBaseFee = true
	}

	if cmd.IsSet("fee_rate_ppm") {
		req.FeeRate = cmd.Uint64("fee_rate_ppm")
		req.UseFeeRate = true
	}

	req.Private = cmd.Bool("private")

	// Parse the channel type and map it to its RPC representation.
	channelType := cmd.String("channel_type")
	switch channelType {
	case "":
		break
	case channelTypeTweakless:
		req.CommitmentType = looprpc.CommitmentType_STATIC_REMOTE_KEY

	case channelTypeAnchors:
		req.CommitmentType = looprpc.CommitmentType_ANCHORS

	case channelTypeSimpleTaproot:
		req.CommitmentType = looprpc.CommitmentType_SIMPLE_TAPROOT
	default:
		return fmt.Errorf("unsupported channel type %v", channelType)
	}

	resp, err := client.StaticOpenChannel(ctxb, req)

	printRespJSON(resp)

	return err
}

// UtxosToOutpoints converts a slice of UTXO strings into a slice of OutPoint
// protobuf objects. It returns an error if no UTXOs are specified or if any
// UTXO string cannot be parsed into an OutPoint.
func UtxosToOutpoints(utxos []string) ([]*looprpc.OutPoint, error) {
	var outpoints []*looprpc.OutPoint
	if len(utxos) == 0 {
		return nil, fmt.Errorf("no utxos specified")
	}
	for _, utxo := range utxos {
		outpoint, err := NewProtoOutPoint(utxo)
		if err != nil {
			return nil, err
		}
		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

// checkNotBothSet accepts two flag names, a and b, and checks that only flag a
// or flag b can be set, but not both. It returns the name of the flag or an
// error.
func checkNotBothSet(cmd *cli.Command, a, b string) (string, error) {
	if cmd.IsSet(a) && cmd.IsSet(b) {
		return "", fmt.Errorf(
			"either %s or %s should be set, but not both", a, b,
		)
	}

	if cmd.IsSet(a) {
		return a, nil
	}

	return b, nil
}
