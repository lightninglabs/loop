package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightninglabs/loop/looprpc"
	"github.com/urfave/cli"
	"gopkg.in/macaroon.v2"
)

type printableToken struct {
	ID              string `json:"id"`
	ValidUntil      string `json:"valid_until"`
	BaseMacaroon    string `json:"base_macaroon"`
	PaymentHash     string `json:"payment_hash"`
	PaymentPreimage string `json:"payment_preimage"`
	AmountPaid      int64  `json:"amount_paid_msat"`
	RoutingFeePaid  int64  `json:"routing_fee_paid_msat"`
	TimeCreated     string `json:"time_created"`
	Expired         bool   `json:"expired"`
	FileName        string `json:"file_name"`
}

var listAuthCommand = cli.Command{
	Name:        "listauth",
	Usage:       "list all L402 tokens",
	Description: "Shows a list of all L402 tokens that loopd has paid for",
	Action:      listAuth,
}

func listAuth(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.GetL402Tokens(
		context.Background(), &looprpc.TokensRequest{},
	)
	if err != nil {
		return err
	}

	tokens := make([]*printableToken, len(resp.Tokens))
	for i, t := range resp.Tokens {
		mac := &macaroon.Macaroon{}
		err := mac.UnmarshalBinary(t.BaseMacaroon)
		if err != nil {
			return fmt.Errorf("unable to unmarshal macaroon: %v",
				err)
		}

		tokens[i] = &printableToken{
			ID:              t.Id,
			ValidUntil:      "",
			BaseMacaroon:    hex.EncodeToString(t.BaseMacaroon),
			PaymentHash:     hex.EncodeToString(t.PaymentHash),
			PaymentPreimage: hex.EncodeToString(t.PaymentPreimage),
			AmountPaid:      t.AmountPaidMsat,
			RoutingFeePaid:  t.RoutingFeePaidMsat,
			TimeCreated: time.Unix(t.TimeCreated, 0).Format(
				time.RFC3339,
			),
			Expired:  t.Expired,
			FileName: t.StorageName,
		}
	}

	printJSON(tokens)
	return nil
}

var fetchL402Command = cli.Command{
	Name:  "fetchl402",
	Usage: "fetches a new L402 authentication token from the server",
	Description: "Fetches a new L402 authentication token from the server. " +
		"This token is required to listen to notifications from the server, " +
		"such as reservation notifications. If a L402 is already present in " +
		"the store, this command is a no-op.",
	Action: fetchL402,
}

func fetchL402(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := client.FetchL402Token(
		context.Background(), &looprpc.FetchL402TokenRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(res)
	return nil
}
