package tapsdk

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultInitialBackoff is the default delay before the first
	// reconnection attempt.
	defaultInitialBackoff = 1 * time.Second

	// defaultMaxBackoff caps the exponential backoff.
	defaultMaxBackoff = 60 * time.Second

	// defaultBackoffMultiplier is the factor applied after each
	// retry.
	defaultBackoffMultiplier = 2.0

	// jitterFraction is the ±fraction of the backoff added as
	// jitter.
	jitterFraction = 0.2
)

// EventHandler holds optional callbacks for asset events. Nil handlers
// are skipped — the listener will not subscribe to event types that
// have no handler registered.
type EventHandler struct {
	// OnReceive is called for each incoming asset transfer event.
	OnReceive func(ctx context.Context, event *ReceiveEvent)

	// OnSend is called for each outgoing asset transfer event.
	OnSend func(ctx context.Context, event *SendEvent)

	// OnMint is called for each minting batch lifecycle event.
	OnMint func(ctx context.Context, event *MintEvent)

	// OnError is called when a stream encounters a non-recoverable
	// error after all retry attempts are exhausted, or when a fatal
	// (non-retriable) error occurs. If nil, errors are silently
	// dropped and the stream is abandoned.
	OnError func(streamName string, err error)

	// OnDisconnect is called on every retriable stream break,
	// before the next reconnection attempt. err is the terminal
	// error returned by the stream (may be nil on a clean server
	// close) and nextRetry is the delay (with jitter) the listener
	// will wait before retrying. Intended for observability — to
	// surface transient hiccups as logs or metrics — while OnError
	// reports the terminal case.
	OnDisconnect func(streamName string, err error,
		nextRetry time.Duration)
}

// EventListenerConfig configures reconnection and subscription
// behavior.
type EventListenerConfig struct {
	// MaxRetries is the maximum number of consecutive reconnection
	// attempts per stream before calling OnError. Zero means
	// unlimited retries.
	MaxRetries int

	// InitialBackoff is the delay before the first reconnection
	// attempt. Defaults to 1 second.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential backoff. Defaults to 60
	// seconds.
	MaxBackoff time.Duration

	// BackoffMultiplier is the factor applied after each retry.
	// Defaults to 2.0.
	BackoffMultiplier float64

	// ReceiveFilter optionally restricts receive events.
	ReceiveFilter *SubscribeReceiveEventsRequest

	// SendFilter optionally restricts send events.
	SendFilter *SubscribeSendEventsRequest

	// MintFilter optionally restricts mint events.
	MintFilter *SubscribeMintEventsRequest
}

// EventListenerOption is a functional option for configuring
// EventListenerConfig.
type EventListenerOption func(*EventListenerConfig)

// WithMaxRetries sets the maximum consecutive reconnection attempts.
func WithMaxRetries(n int) EventListenerOption {
	return func(c *EventListenerConfig) {
		c.MaxRetries = n
	}
}

// WithInitialBackoff sets the initial reconnection backoff duration.
func WithInitialBackoff(d time.Duration) EventListenerOption {
	return func(c *EventListenerConfig) {
		c.InitialBackoff = d
	}
}

// WithMaxBackoff sets the maximum backoff duration.
func WithMaxBackoff(d time.Duration) EventListenerOption {
	return func(c *EventListenerConfig) {
		c.MaxBackoff = d
	}
}

// WithBackoffMultiplier sets the backoff growth factor.
func WithBackoffMultiplier(m float64) EventListenerOption {
	return func(c *EventListenerConfig) {
		c.BackoffMultiplier = m
	}
}

// WithReceiveFilter sets the receive event subscription filter.
func WithReceiveFilter(
	f *SubscribeReceiveEventsRequest) EventListenerOption {

	return func(c *EventListenerConfig) {
		c.ReceiveFilter = f
	}
}

// WithSendFilter sets the send event subscription filter.
func WithSendFilter(
	f *SubscribeSendEventsRequest) EventListenerOption {

	return func(c *EventListenerConfig) {
		c.SendFilter = f
	}
}

// WithMintFilter sets the mint event subscription filter.
func WithMintFilter(
	f *SubscribeMintEventsRequest) EventListenerOption {

	return func(c *EventListenerConfig) {
		c.MintFilter = f
	}
}

// EventListener subscribes to tapd event streams and dispatches events
// to the registered handlers, reconnecting with exponential backoff on
// retriable failures.
type EventListener struct {
	client  EventClient
	handler EventHandler
	config  EventListenerConfig

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// NewEventListener creates an EventListener backed by the given
// client. The client can be either a grpc.Client or rest.Client —
// both satisfy the EventClient interface.
func NewEventListener(
	client EventClient,
	handler EventHandler,
	opts ...EventListenerOption,
) *EventListener {

	cfg := EventListenerConfig{
		InitialBackoff:    defaultInitialBackoff,
		MaxBackoff:        defaultMaxBackoff,
		BackoffMultiplier: defaultBackoffMultiplier,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return &EventListener{
		client:  client,
		handler: handler,
		config:  cfg,
	}
}

// Start begins listening for events. It launches background goroutines
// for each registered handler. Start is non-blocking and returns
// immediately. Returns an error if the listener is already running.
func (l *EventListener) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		return errors.New("event listener already running")
	}

	ctx, l.cancel = context.WithCancel(ctx)
	l.running = true

	if l.handler.OnReceive != nil {
		l.wg.Add(1)
		go l.runStream(ctx, "receive", l.subscribeReceive)
	}

	if l.handler.OnSend != nil {
		l.wg.Add(1)
		go l.runStream(ctx, "send", l.subscribeSend)
	}

	if l.handler.OnMint != nil {
		l.wg.Add(1)
		go l.runStream(ctx, "mint", l.subscribeMint)
	}

	return nil
}

// Stop gracefully shuts down all subscriptions and waits for goroutines
// to exit. Safe to call multiple times.
func (l *EventListener) Stop() error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	l.cancel()
	l.mu.Unlock()

	l.wg.Wait()

	l.mu.Lock()
	l.running = false
	l.mu.Unlock()

	return nil
}

// Running reports whether the listener is currently active.
func (l *EventListener) Running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

// streamResult is the outcome of a single subscription attempt.
type streamResult struct {
	// err is the terminal error that ended the stream.
	err error

	// delivered is the number of events successfully dispatched
	// to handlers before the stream broke.
	delivered int
}

// streamFunc opens a subscription and blocks until the stream ends
// or ctx is cancelled.
type streamFunc func(ctx context.Context) streamResult

// runStream manages reconnection for a single event stream.
func (l *EventListener) runStream(ctx context.Context, name string,
	subscribe streamFunc) {

	defer l.wg.Done()

	backoff := l.config.InitialBackoff
	retries := 0

	for {
		result := subscribe(ctx)

		// Context cancelled: clean shutdown.
		if ctx.Err() != nil {
			return
		}

		// Check if the error is fatal (non-retriable).
		if isFatalError(result.err) {
			if l.handler.OnError != nil {
				l.handler.OnError(name, result.err)
			}
			return
		}

		// Successful event delivery resets the retry
		// counter and backoff. A stream that reconnected
		// and delivered at least one event is healthy.
		if result.delivered > 0 {
			retries = 0
			backoff = l.config.InitialBackoff
		}

		retries++

		// Check retry limit.
		if l.config.MaxRetries > 0 &&
			retries > l.config.MaxRetries {

			if l.handler.OnError != nil {
				l.handler.OnError(name, result.err)
			}
			return
		}

		// Sleep with jitter.
		jittered := addJitter(backoff)

		// Notify observers of the transient break before
		// waiting. Callers use this for retry metrics or logs.
		if l.handler.OnDisconnect != nil {
			l.handler.OnDisconnect(name, result.err, jittered)
		}

		select {
		case <-time.After(jittered):
		case <-ctx.Done():
			return
		}

		// Grow backoff.
		backoff = min(
			time.Duration(
				float64(backoff)*
					l.config.BackoffMultiplier,
			),
			l.config.MaxBackoff,
		)
	}
}

// subscribeReceive opens a receive event stream and delivers events
// to the handler until the stream breaks. The raw record from the
// client is projected into the high-level ReceiveEvent before dispatch
// so handlers operate on AssetRef-keyed shapes.
func (l *EventListener) subscribeReceive(
	ctx context.Context) streamResult {

	filter := l.config.ReceiveFilter
	if filter == nil {
		filter = &SubscribeReceiveEventsRequest{}
	}

	eventCh, errCh, err := l.client.SubscribeReceiveEvents(
		ctx, filter,
	)
	if err != nil {
		return streamResult{err: err}
	}

	return drainEvents(ctx, eventCh, errCh,
		func(record *ReceiveEventRecord) {
			l.handler.OnReceive(ctx, NewReceiveEvent(
				record,
			))
		},
	)
}

// subscribeSend opens a send event stream and delivers events. The raw
// record from the client is projected into the high-level SendEvent
// before dispatch so handlers operate on AssetRef-keyed shapes.
func (l *EventListener) subscribeSend(
	ctx context.Context) streamResult {

	filter := l.config.SendFilter
	if filter == nil {
		filter = &SubscribeSendEventsRequest{}
	}

	eventCh, errCh, err := l.client.SubscribeSendEvents(
		ctx, filter,
	)
	if err != nil {
		return streamResult{err: err}
	}

	return drainEvents(ctx, eventCh, errCh,
		func(record *SendEventRecord) {
			l.handler.OnSend(ctx, NewSendEvent(record))
		},
	)
}

// subscribeMint opens a mint event stream and delivers events.
func (l *EventListener) subscribeMint(
	ctx context.Context) streamResult {

	filter := l.config.MintFilter
	if filter == nil {
		filter = &SubscribeMintEventsRequest{}
	}

	eventCh, errCh, err := l.client.SubscribeMintEvents(
		ctx, filter,
	)
	if err != nil {
		return streamResult{err: err}
	}

	return drainEvents(ctx, eventCh, errCh,
		func(e *MintEvent) {
			l.handler.OnMint(ctx, e)
		},
	)
}

// drainEvents reads from an event channel and calls handler for each
// event. Returns a streamResult with the terminal error and the count
// of events delivered.
//
// Events are always prioritised over the error channel: when both
// are ready, any buffered events are delivered first. This prevents
// the Go select race where errCh wins and causes event loss.
func drainEvents[T any](ctx context.Context,
	eventCh <-chan *T, errCh <-chan error,
	handler func(*T)) streamResult {

	delivered := 0

	for {
		// Priority: deliver pending events before checking
		// errors. This avoids the select race where errCh wins
		// and buffered events are lost.
		select {
		case event, ok := <-eventCh:
			if !ok {
				// Event channel closed, read terminal
				// error.
				select {
				case err := <-errCh:
					return streamResult{
						err:       err,
						delivered: delivered,
					}
				default:
					return streamResult{
						delivered: delivered,
					}
				}
			}
			handler(event)
			delivered++
			continue

		default:
		}

		// No pending event, wait on all channels.
		select {
		case event, ok := <-eventCh:
			if !ok {
				select {
				case err := <-errCh:
					return streamResult{
						err:       err,
						delivered: delivered,
					}
				default:
					return streamResult{
						delivered: delivered,
					}
				}
			}
			handler(event)
			delivered++

		case err := <-errCh:
			// Drain any remaining buffered events
			// before returning the error.
			for {
				select {
				case event, ok := <-eventCh:
					if !ok {
						return streamResult{
							err:       err,
							delivered: delivered,
						}
					}
					handler(event)
					delivered++
				default:
					return streamResult{
						err:       err,
						delivered: delivered,
					}
				}
			}

		case <-ctx.Done():
			return streamResult{
				err:       ctx.Err(),
				delivered: delivered,
			}
		}
	}
}

// isFatalError returns true if the error should not be retried.
func isFatalError(err error) bool {
	if err == nil {
		return false
	}

	// gRPC status codes that are fatal.
	s, ok := status.FromError(err)
	if ok {
		switch s.Code() {
		case codes.PermissionDenied,
			codes.Unauthenticated,
			codes.Unimplemented,
			codes.InvalidArgument:

			return true
		}
	}

	return false
}

// addJitter adds ±jitterFraction random jitter to a duration.
func addJitter(d time.Duration) time.Duration {
	jitter := float64(d) * jitterFraction
	offset := (cryptoFloat64()*2 - 1) * jitter

	return time.Duration(math.Max(
		float64(d)+offset,
		float64(time.Millisecond),
	))
}

// cryptoFloat64 returns a cryptographically random float64 in
// [0, 1).
func cryptoFloat64() float64 {
	var buf [8]byte
	_, _ = rand.Read(buf[:])

	return float64(
		binary.LittleEndian.Uint64(buf[:])>>11,
	) / (1 << 53)
}
