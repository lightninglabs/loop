package test

import (
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lntypes"
	"golang.org/x/net/context"
)

type mockRouter struct {
	lndclient.RouterClient
	lnd *LndMockServices
}

func (r *mockRouter) SendPayment(ctx context.Context,
	request lndclient.SendPaymentRequest) (chan lndclient.PaymentStatus,
	chan error, error) {

	statusChan := make(chan lndclient.PaymentStatus)
	errorChan := make(chan error)

	r.lnd.RouterSendPaymentChannel <- RouterPaymentChannelMessage{
		SendPaymentRequest: request,
		TrackPaymentMessage: TrackPaymentMessage{
			Updates: statusChan,
			Errors:  errorChan,
		},
	}

	return statusChan, errorChan, nil
}

func (r *mockRouter) TrackPayment(ctx context.Context,
	hash lntypes.Hash) (chan lndclient.PaymentStatus, chan error, error) {

	statusChan := make(chan lndclient.PaymentStatus)
	errorChan := make(chan error)
	r.lnd.TrackPaymentChannel <- TrackPaymentMessage{
		Hash:    hash,
		Updates: statusChan,
		Errors:  errorChan,
	}

	return statusChan, errorChan, nil
}

func (r *mockRouter) QueryMissionControl(ctx context.Context) (
	[]lndclient.MissionControlEntry, error) {

	return r.lnd.MissionControlState, nil
}

// ImportMissionControl is a mocked reimplementation of the pair import.
// Reference: lnd/router/missioncontrol_state.go:importSnapshot().
func (r *mockRouter) ImportMissionControl(ctx context.Context,
	entries []lndclient.MissionControlEntry, force bool) error {

	for _, entry := range entries {
		found := false
		for i := range r.lnd.MissionControlState {
			current := &r.lnd.MissionControlState[i]
			if entry.NodeFrom == current.NodeFrom &&
				entry.NodeTo == current.NodeTo {

				// Mark that the entry has been found and updated.
				found = true

				// Import failure result first. We ignore failure
				// relax interval here for convenience.
				current.FailTime = entry.FailTime
				current.FailAmt = entry.FailAmt

				switch {
				case entry.FailAmt == 0:
					current.SuccessAmt = 0

				case entry.FailAmt <= current.SuccessAmt:
					current.SuccessAmt = entry.FailAmt - 1
				}

				// Import success result second.
				current.SuccessTime = entry.SuccessTime
				if force ||
					entry.SuccessAmt > current.SuccessAmt {

					current.SuccessAmt = entry.SuccessAmt
				}

				if !force && (!current.FailTime.IsZero() &&
					entry.SuccessAmt >= current.FailAmt) {

					current.FailAmt = entry.SuccessAmt + 1
				}
			}
		}

		if !found {
			r.lnd.MissionControlState = append(
				r.lnd.MissionControlState, entry,
			)
		}
	}

	return nil
}

func (r *mockRouter) ResetMissionControl(ctx context.Context) error {
	r.lnd.MissionControlState = []lndclient.MissionControlEntry{}
	return nil
}
