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
	Usage:       "list all LSAT tokens",
	Description: "Shows a list of all LSAT tokens that loopd has paid for",
	Action:      listAuth,
}

func listAuth(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.GetLsatTokens(
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
