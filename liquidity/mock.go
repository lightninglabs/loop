package liquidity

import (
	"context"

	"github.com/lightninglabs/loop"
	"github.com/stretchr/testify/mock"
)

// newMockConfig returns a liquidity config with mocked calls. Note that
// functions that are not implemented by the mock will panic if called.
func newMockConfig() (*mockCfg, *Config) {
	mockCfg := &mockCfg{}

	// Create a liquidity config which calls our mock.
	config := &Config{
		LoopInQuote: mockCfg.LoopInQuote,
	}

	return mockCfg, config
}

type mockCfg struct {
	mock.Mock
}

// LoopInQuote mocks a call to get a loop in quote from the server.
func (m *mockCfg) LoopInQuote(ctx context.Context,
	request *loop.LoopInQuoteRequest) (*loop.LoopInQuote, error) {

	args := m.Called(ctx, request)
	return args.Get(0).(*loop.LoopInQuote), args.Error(1)
}
