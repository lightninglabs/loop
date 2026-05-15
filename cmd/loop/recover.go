package main

import (
	"context"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli/v3"
)

var recoverCommand = &cli.Command{
	Name:  "recover",
	Usage: "restore static address and L402 state from a local backup file",
	Description: "Restores the local static-address state and L402 token " +
		"from an encrypted backup file. If --backup_file is omitted, " +
		"loopd selects the latest decryptable active-network backup " +
		"candidate and fully validates it before restoring state.",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name: "backup_file",
			Usage: "path to an encrypted backup file; if omitted, " +
				"loopd selects and validates the latest active-network " +
				"backup candidate",
		},
	},
	Action: runRecover,
}

func runRecover(ctx context.Context, cmd *cli.Command) error {
	if cmd.NArg() > 0 {
		return showCommandHelp(ctx, cmd)
	}

	client, cleanup, err := getClient(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.Recover(
		ctx, &looprpc.RecoverRequest{
			BackupFile: cmd.String("backup_file"),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
