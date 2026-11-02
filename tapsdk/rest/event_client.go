package rest

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"google.golang.org/grpc/codes"
)

// maxWSMessageSize caps a single JSON event frame at 4 MiB, matching
// lnd's WebSocket proxy limit. Large send events carry virtual PSBT
// blobs, so the smaller default would reject them.
const maxWSMessageSize = 4 * 1024 * 1024

// eventClient implements tapsdk.EventClient over REST using tapd's
// WebSocket streaming bridge on top of the gRPC-gateway. Each
// subscription opens its own WS connection and receives
// newline-delimited JSON envelopes of the form:
//
//	{"result": {...event JSON...}}
//	{"error":  {...gateway error envelope...}}
type eventClient struct {
	baseURL   string
	tlsCfg    *tls.Config
	macaroons macaroon.Pouch
}

func newEventClient(tp *transport) *eventClient {
	return &eventClient{
		baseURL:   tp.baseURL,
		tlsCfg:    tp.tlsCfg,
		macaroons: tp.macaroons,
	}
}

// wsStreamError is the streaming error envelope emitted by the
// grpc-gateway wrapper when a server-streaming RPC fails mid-stream.
type wsStreamError struct {
	GRPCCode   int    `json:"grpc_code"`
	HTTPCode   int    `json:"http_code"`
	Message    string `json:"message"`
	HTTPStatus string `json:"http_status"`
}

// wsEnvelope is the per-message wrapper the grpc-gateway emits on the
// WebSocket stream.
type wsEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  *wsStreamError  `json:"error"`
}

// SubscribeReceiveEvents opens a WebSocket subscription for incoming
// asset transfer events.
func (c *eventClient) SubscribeReceiveEvents(ctx context.Context,
	req *tapsdk.SubscribeReceiveEventsRequest) (
	<-chan *tapsdk.ReceiveEventRecord, <-chan error, error) {

	body := &jsonSubscribeReceiveEventsRequest{}
	if req != nil {
		body.FilterAddr = req.FilterAddr
		if req.StartTimestamp != 0 {
			body.StartTimestamp = fmt.Sprintf(
				"%d", req.StartTimestamp,
			)
		}
	}

	return streamEvents(
		ctx, c, "/v1/taproot-assets/events/asset-receive",
		macaroon.AdminServiceMac, body,
		func(raw json.RawMessage) (*tapsdk.ReceiveEventRecord,
			error) {

			var ev jsonReceiveEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				return nil, err
			}

			return unmarshalReceiveEvent(&ev)
		},
	)
}

// SubscribeSendEvents opens a WebSocket subscription for outgoing
// asset transfer events.
func (c *eventClient) SubscribeSendEvents(ctx context.Context,
	req *tapsdk.SubscribeSendEventsRequest) (
	<-chan *tapsdk.SendEventRecord, <-chan error, error) {

	body := &jsonSubscribeSendEventsRequest{}
	if req != nil {
		// tapd's REST gateway is configured with UseHexForBytes,
		// so proto `bytes` fields are hex-encoded on the wire.
		if len(req.FilterScriptKey) > 0 {
			body.FilterScriptKey = hex.EncodeToString(
				req.FilterScriptKey,
			)
		}
		body.FilterLabel = req.FilterLabel
		if req.StartTimestamp != 0 {
			body.StartTimestamp = fmt.Sprintf(
				"%d", req.StartTimestamp,
			)
		}
	}

	return streamEvents(
		ctx, c, "/v1/taproot-assets/events/asset-send",
		macaroon.AdminServiceMac, body,
		func(raw json.RawMessage) (*tapsdk.SendEventRecord,
			error) {

			var ev jsonSendEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				return nil, err
			}

			return unmarshalSendEvent(&ev)
		},
	)
}

// SubscribeMintEvents opens a WebSocket subscription for minting batch
// lifecycle events.
func (c *eventClient) SubscribeMintEvents(ctx context.Context,
	req *tapsdk.SubscribeMintEventsRequest) (
	<-chan *tapsdk.MintEvent, <-chan error, error) {

	body := &jsonSubscribeMintEventsRequest{}
	if req != nil {
		body.ShortResponse = req.ShortResponse
	}

	return streamEvents(
		ctx, c, "/v1/taproot-assets/events/asset-mint",
		macaroon.MintServiceMac, body,
		func(raw json.RawMessage) (*tapsdk.MintEvent, error) {
			var ev jsonMintEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				return nil, err
			}

			return unmarshalMintEvent(&ev)
		},
	)
}

// streamEvents opens a WebSocket subscription against the tapd REST
// proxy, sends the JSON request body as the first WS frame, and
// forwards decoded events on an outgoing channel until the stream
// terminates. A terminal error (stream error, decode failure, or
// context cancellation) is published on errCh; a clean server close
// closes both channels without emitting an error.
func streamEvents[T any](ctx context.Context, c *eventClient,
	path string, mac macaroon.TaprpcServiceMac, body any,
	decode func(json.RawMessage) (*T, error)) (
	<-chan *T, <-chan error, error) {

	wsURL, err := websocketURL(c.baseURL, path)
	if err != nil {
		return nil, nil, err
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: c.tlsCfg,
		},
	}

	header := http.Header{}
	header.Set(macaroonHeader, string(c.macaroons[mac]))

	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: header,
	})
	if resp != nil && resp.Body != nil {
		// coder/websocket returns the upgrade response so callers
		// can inspect its headers. The body is already drained on
		// a successful upgrade, but the contract still requires us
		// to close it.
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("websocket dial %s: %w",
			path, err)
	}

	conn.SetReadLimit(maxWSMessageSize)

	reqJSON, err := json.Marshal(body)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "bad request")

		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	err = conn.Write(ctx, websocket.MessageText, reqJSON)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "write failed")

		return nil, nil, fmt.Errorf("websocket write: %w", err)
	}

	eventCh := make(chan *T)
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)
		defer close(errCh)
		defer func() {
			// A best-effort clean close; the context-cancel
			// path will already have closed it in most cases.
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		}()

		for {
			_, msg, err := conn.Read(ctx)
			if err != nil {
				// Clean server close — no event, no error.
				if websocket.CloseStatus(err) ==
					websocket.StatusNormalClosure ||
					websocket.CloseStatus(err) ==
						websocket.StatusGoingAway {

					return
				}
				if ctx.Err() != nil {
					errCh <- ctx.Err()
					return
				}

				errCh <- fmt.Errorf("websocket read: %w",
					err)
				return
			}

			var env wsEnvelope
			if err := json.Unmarshal(msg, &env); err != nil {
				errCh <- fmt.Errorf("decode envelope: %w",
					err)
				return
			}

			if env.Error != nil {
				errCh <- &APIError{
					StatusCode: env.Error.HTTPCode,
					GRPCCode: codes.Code(
						env.Error.GRPCCode,
					),
					Message: env.Error.Message,
				}
				return
			}

			if len(env.Result) == 0 {
				continue
			}

			event, err := decode(env.Result)
			if err != nil {
				errCh <- fmt.Errorf("unmarshal event: %w",
					err)
				return
			}

			select {
			case eventCh <- event:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	return eventCh, errCh, nil
}

// websocketURL converts the REST base URL plus an RPC path into the
// wss:// URL expected by tapd's WebSocket proxy. The gRPC-gateway streaming
// bridge requires the RPC method to be carried as a query parameter because
// the WS upgrade itself is always GET.
func websocketURL(baseURL, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}

	if u.Scheme != "https" {
		return "", fmt.Errorf("unsupported base URL scheme: %s",
			u.Scheme)
	}
	u.Scheme = "wss"

	u.Path = strings.TrimRight(u.Path, "/") + path

	q := u.Query()
	q.Set("method", http.MethodPost)
	u.RawQuery = q.Encode()

	return u.String(), nil
}
