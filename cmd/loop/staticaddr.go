package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
)

var staticAddressCommands = cli.Command{
	Name:      "static",
	ShortName: "s",
	Usage:     "manage static loop-in addresses",
	Category:  "StaticAddress",
	Subcommands: []cli.Command{
		newStaticAddressCommand,
		listUnspentCommand,
	},
}

var newStaticAddressCommand = cli.Command{
	Name:      "new",
	ShortName: "n",
	Usage:     "Create a new static loop in address.",
	Description: `
	Requests a new static loop in address from the server. Funds that are
	sent to this address will be locked by a 2:2 multisig between us and the
	loop server, or a timeout path that we can sweep once it opens up. The 
	funds can either be cooperatively spent with a signature from the server
	or looped in.
	`,
	Action: newStaticAddress,
}

func newStaticAddress(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "new")
	}

	client, cleanup, err := getAddressClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.NewAddress(
		ctxb, &looprpc.NewAddressRequest{},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Received a new static loop-in address from the server: "+
		"%s\n", resp.Address)

	return nil
}

var listUnspentCommand = cli.Command{
	Name:      "listunspent",
	ShortName: "l",
	Usage:     "List unspent static address outputs.",
	Description: `
	List all unspent static address outputs.
	`,
	Flags: []cli.Flag{
		cli.IntFlag{
			Name: "min_confs",
			Usage: "The minimum amount of confirmations an " +
				"output should have to be listed.",
		},
		cli.IntFlag{
			Name: "max_confs",
			Usage: "The maximum number of confirmations an " +
				"output could have to be listed.",
		},
	},
	Action: listUnspent,
}

func listUnspent(ctx *cli.Context) error {
	ctxb := context.Background()
	if ctx.NArg() > 0 {
		return cli.ShowCommandHelp(ctx, "listunspent")
	}

	client, cleanup, err := getAddressClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListUnspent(ctxb, &looprpc.ListUnspentRequest{
		MinConfs: int32(ctx.Int("min_confs")),
		MaxConfs: int32(ctx.Int("max_confs")),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

func getAddressClient(ctx *cli.Context) (looprpc.StaticAddressClientClient,
	func(), error) {

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

	addressClient := looprpc.NewStaticAddressClientClient(conn)
	return addressClient, cleanup, nil
}
