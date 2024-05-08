package fsm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	errService = errors.New("service error")
	errStore   = errors.New("store error")
)

type mockStore struct {
	storeErr error
}

func (m *mockStore) StoreStuff() error {
	return m.storeErr
}

type mockService struct {
	respondChan chan bool
	respondErr  error
}

func (m *mockService) WaitForStuffHappening() (<-chan bool, error) {
	return m.respondChan, m.respondErr
}

func newInitStuffRequest() *InitStuffRequest {
	return &InitStuffRequest{
		Stuff:       "stuff",
		respondChan: make(chan<- string, 1),
	}
}

func TestExampleFSM(t *testing.T) {
	testCases := []struct {
		name                    string
		expectedState           StateType
		eventCtx                EventContext
		expectedLastActionError error

		sendEvent    EventType
		sendEventErr error

		serviceErr error
		storeErr   error
	}{
		{
			name:          "success",
			expectedState: StuffSuccess,
			eventCtx:      newInitStuffRequest(),
			sendEvent:     OnRequestStuff,
		},
		{
			name:                    "service error",
			expectedState:           StuffFailed,
			eventCtx:                newInitStuffRequest(),
			sendEvent:               OnRequestStuff,
			serviceErr:              errService,
			expectedLastActionError: errService,
		},
		{
			name:                    "store error",
			expectedLastActionError: errStore,
			storeErr:                errStore,
			sendEvent:               OnRequestStuff,
			expectedState:           StuffFailed,
			eventCtx:                newInitStuffRequest(),
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			respondChan := make(chan string, 1)
			if req, ok := tc.eventCtx.(*InitStuffRequest); ok {
				req.respondChan = respondChan
			}

			serviceResponseChan := make(chan bool, 1)
			serviceResponseChan <- true

			service := &mockService{
				respondChan: serviceResponseChan,
				respondErr:  tc.serviceErr,
			}

			store := &mockStore{
				storeErr: tc.storeErr,
			}

			exampleContext := NewExampleFSMContext(service, store)
			cachedObserver := NewCachedObserver(100)

			exampleContext.RegisterObserver(cachedObserver)

			err := exampleContext.SendEvent(
				tc.sendEvent, tc.eventCtx,
			)
			require.Equal(t, tc.sendEventErr, err)

			require.Equal(
				t,
				tc.expectedLastActionError,
				exampleContext.LastActionError,
			)

			err = cachedObserver.WaitForState(
				context.Background(),
				time.Second,
				tc.expectedState,
			)
			require.NoError(t, err)
		})
	}
}

// getTestContext returns a test context for the example FSM and a cached
// observer that can be used to verify the state transitions.
func getTestContext() (*ExampleFSM, *CachedObserver) {
	service := &mockService{
		respondChan: make(chan bool, 1),
	}
	service.respondChan <- true

	store := &mockStore{}

	exampleContext := NewExampleFSMContext(service, store)
	cachedObserver := NewCachedObserver(100)

	exampleContext.RegisterObserver(cachedObserver)

	return exampleContext, cachedObserver
}

// TestExampleFSMFlow tests different flows that the example FSM can go through.
func TestExampleFSMFlow(t *testing.T) {
	testCases := []struct {
		name              string
		expectedStateFlow []StateType
		expectedEventFlow []EventType
		storeError        error
		serviceError      error
	}{
		{
			name: "success",
			expectedStateFlow: []StateType{
				InitFSM,
				StuffSentOut,
				StuffSuccess,
			},
			expectedEventFlow: []EventType{
				OnRequestStuff,
				OnStuffSentOut,
				OnStuffSuccess,
			},
		},
		{
			name: "failure on store",
			expectedStateFlow: []StateType{
				InitFSM,
				StuffFailed,
			},
			expectedEventFlow: []EventType{
				OnRequestStuff,
				OnError,
			},
			storeError: errStore,
		},
		{
			name: "failure on service",
			expectedStateFlow: []StateType{
				InitFSM,
				StuffSentOut,
				StuffFailed,
			},
			expectedEventFlow: []EventType{
				OnRequestStuff,
				OnStuffSentOut,
				OnError,
			},
			serviceError: errService,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			exampleContext, cachedObserver := getTestContext()

			if tc.storeError != nil {
				exampleContext.store.(*mockStore).
					storeErr = tc.storeError
			}

			if tc.serviceError != nil {
				exampleContext.service.(*mockService).
					respondErr = tc.serviceError
			}

			go func() {
				err := exampleContext.SendEvent(
					OnRequestStuff,
					newInitStuffRequest(),
				)

				require.NoError(t, err)
			}()

			// Wait for the final state.
			err := cachedObserver.WaitForState(
				context.Background(),
				time.Second,
				tc.expectedStateFlow[len(
					tc.expectedStateFlow,
				)-1],
			)
			require.NoError(t, err)

			allNotifications := cachedObserver.
				GetCachedNotifications()

			for index, notification := range allNotifications {
				require.Equal(
					t,
					tc.expectedStateFlow[index],
					notification.NextState,
				)
				require.Equal(
					t,
					tc.expectedEventFlow[index],
					notification.Event,
				)
			}
		})
	}
}

// TestObserverAsyncWait tests the observer's WaitForStateAsync function.
func TestObserverAsyncWait(t *testing.T) {
	testCases := []struct {
		name          string
		waitTime      time.Duration
		blockTime     time.Duration
		expectTimeout bool
	}{
		{
			name:          "success",
			waitTime:      time.Second,
			blockTime:     time.Millisecond,
			expectTimeout: false,
		},
		{
			name:          "timeout",
			waitTime:      time.Millisecond,
			blockTime:     time.Second,
			expectTimeout: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			service := &mockService{
				respondChan: make(chan bool),
			}

			store := &mockStore{}

			exampleContext := NewExampleFSMContext(service, store)
			cachedObserver := NewCachedObserver(100)
			exampleContext.RegisterObserver(cachedObserver)

			t0 := time.Now()
			timeoutCtx, cancel := context.WithTimeout(
				context.Background(), tc.waitTime,
			)
			defer cancel()

			// Wait for the final state.
			errChan := cachedObserver.WaitForStateAsync(
				timeoutCtx, StuffSuccess, true,
			)

			go func() {
				err := exampleContext.SendEvent(
					OnRequestStuff,
					newInitStuffRequest(),
				)

				require.NoError(t, err)

				time.Sleep(tc.blockTime)
				service.respondChan <- true
			}()

			timeout := false
			select {
			case <-timeoutCtx.Done():
				timeout = true

			case <-errChan:
			}
			require.Equal(t, tc.expectTimeout, timeout)

			t1 := time.Now()
			diff := t1.Sub(t0)
			if tc.expectTimeout {
				require.Less(t, diff, tc.blockTime)
			} else {
				require.Less(t, diff, tc.waitTime)
			}
		})
	}
}
