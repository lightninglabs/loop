package loopd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

const (
	// testMockLndHeight is the block height the mock lnd node reports.
	testMockLndHeight = 600

	// testMockLndPubkey is the identity pubkey the mock lnd node
	// reports.
	testMockLndPubkey = "03f5374b16f0b1f1b49101de1b9d89e0b460bc57ce9c2f9" +
		"132b73dfc76d3704daa"

	// testMacaroonHex is a pre-baked macaroon (root key
	// "dummy-root-key", id "42", location "lnd") that is used as the
	// macaroon for all lnd services. The mock lnd server does not
	// validate macaroons, the hex only needs to parse.
	testMacaroonHex = "0201036c6e6402023432000006205a6adf86f5fef1c3738c7" +
		"c299da15c0bb5e2c3d3f84e89090bbad9e070ee6663"

	// testTimeout is the maximum time we wait for the daemon to shut
	// down.
	testTimeout = time.Second * 30

	// testMockLndName is the alias and TLS common name of the mock lnd
	// node.
	testMockLndName = "mock-lnd"
)

// The following mock servers implement the minimal subset of lnd's gRPC
// services that is required to create an lndclient.GrpcLndServices and to
// start an embedded loop daemon. They are used to verify that the injected
// lnd connection is not closed by the subserver on shutdown. Each service
// gets its own struct because the embedded Unimplemented types have
// overlapping method names.

// mockLightningServer implements lnd's main Lightning gRPC service.
type mockLightningServer struct {
	lnrpc.UnimplementedLightningServer
}

// GetInfo returns static node information.
func (m *mockLightningServer) GetInfo(_ context.Context,
	_ *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	return &lnrpc.GetInfoResponse{
		IdentityPubkey: testMockLndPubkey,
		Alias:          testMockLndName,
		Color:          "#3399ff",
		Version:        "0.21.0-beta",
		BlockHeight:    testMockLndHeight,
		BlockHash: "00000000000000000000000000000000" +
			"00000000000000000000000000000000",
		SyncedToChain: true,
		SyncedToGraph: true,
		Chains: []*lnrpc.Chain{
			{
				Chain:   "bitcoin",
				Network: "testnet",
			},
		},
	}, nil
}

// ListPayments returns an empty payment list.
func (m *mockLightningServer) ListPayments(_ context.Context,
	_ *lnrpc.ListPaymentsRequest) (*lnrpc.ListPaymentsResponse, error) {

	return &lnrpc.ListPaymentsResponse{}, nil
}

// mockStateServer implements lnd's State gRPC service.
type mockStateServer struct {
	lnrpc.UnimplementedStateServer
}

// SubscribeState reports the wallet as unlocked and then blocks.
func (m *mockStateServer) SubscribeState(_ *lnrpc.SubscribeStateRequest,
	stream lnrpc.State_SubscribeStateServer) error {

	err := stream.Send(&lnrpc.SubscribeStateResponse{
		State: lnrpc.WalletState_RPC_ACTIVE,
	})
	if err != nil {
		return err
	}

	<-stream.Context().Done()

	return stream.Context().Err()
}

// GetState reports the wallet as unlocked.
func (m *mockStateServer) GetState(_ context.Context,
	_ *lnrpc.GetStateRequest) (*lnrpc.GetStateResponse, error) {

	return &lnrpc.GetStateResponse{
		State: lnrpc.WalletState_RPC_ACTIVE,
	}, nil
}

// mockVersionerServer implements lnd's verrpc gRPC service.
type mockVersionerServer struct {
	verrpc.UnimplementedVersionerServer
}

// GetVersion returns a version that satisfies the minimum required lnd
// version of the loop daemon.
func (m *mockVersionerServer) GetVersion(_ context.Context,
	_ *verrpc.VersionRequest) (*verrpc.Version, error) {

	return &verrpc.Version{
		AppMajor: 0,
		AppMinor: 21,
		AppPatch: 0,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc",
			"invoicesrpc",
		},
	}, nil
}

// mockChainNotifierServer implements lnd's chainrpc gRPC service.
type mockChainNotifierServer struct {
	chainrpc.UnimplementedChainNotifierServer
}

// RegisterBlockEpochNtfn sends the current block height once (like lnd
// does on registration) and then blocks until the subscription is
// canceled.
func (m *mockChainNotifierServer) RegisterBlockEpochNtfn(
	_ *chainrpc.BlockEpoch,
	stream chainrpc.ChainNotifier_RegisterBlockEpochNtfnServer) error {

	err := stream.Send(&chainrpc.BlockEpoch{
		Height: testMockLndHeight,
	})
	if err != nil {
		return err
	}

	<-stream.Context().Done()

	return stream.Context().Err()
}

// RegisterConfirmationsNtfn blocks until the subscription is canceled.
func (m *mockChainNotifierServer) RegisterConfirmationsNtfn(
	_ *chainrpc.ConfRequest,
	stream chainrpc.ChainNotifier_RegisterConfirmationsNtfnServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// RegisterSpendNtfn blocks until the subscription is canceled.
func (m *mockChainNotifierServer) RegisterSpendNtfn(
	_ *chainrpc.SpendRequest,
	stream chainrpc.ChainNotifier_RegisterSpendNtfnServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// mockWalletKitServer implements lnd's walletrpc gRPC service.
type mockWalletKitServer struct {
	walletrpc.UnimplementedWalletKitServer
}

// ListUnspent returns an empty UTXO list.
func (m *mockWalletKitServer) ListUnspent(_ context.Context,
	_ *walletrpc.ListUnspentRequest) (*walletrpc.ListUnspentResponse,
	error) {

	return &walletrpc.ListUnspentResponse{}, nil
}

// mockInvoicesServer implements lnd's invoicesrpc gRPC service.
type mockInvoicesServer struct {
	invoicesrpc.UnimplementedInvoicesServer
}

// SubscribeSingleInvoice blocks until the subscription is canceled.
func (m *mockInvoicesServer) SubscribeSingleInvoice(
	_ *invoicesrpc.SubscribeSingleInvoiceRequest,
	stream invoicesrpc.Invoices_SubscribeSingleInvoiceServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// startMockLnd starts the mock lnd gRPC server on an in-memory listener and
// returns the listener to dial.
func startMockLnd(t *testing.T) *bufconn.Listener {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.Creds(
		credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{testTLSCert(t)},
		}),
	))

	lnrpc.RegisterLightningServer(server, &mockLightningServer{})
	lnrpc.RegisterStateServer(server, &mockStateServer{})
	verrpc.RegisterVersionerServer(server, &mockVersionerServer{})
	chainrpc.RegisterChainNotifierServer(
		server, &mockChainNotifierServer{},
	)
	walletrpc.RegisterWalletKitServer(server, &mockWalletKitServer{})
	invoicesrpc.RegisterInvoicesServer(server, &mockInvoicesServer{})
	routerrpc.RegisterRouterServer(
		server, &routerrpc.UnimplementedRouterServer{},
	)
	signrpc.RegisterSignerServer(
		server, &signrpc.UnimplementedSignerServer{},
	)

	go func() {
		_ = server.Serve(lis)
	}()

	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})

	return lis
}

// newInjectedLndServices creates a real lndclient.GrpcLndServices backed by
// a real gRPC connection to the mock lnd server. This mirrors exactly what
// LiT creates once and then injects into its subservers.
func newInjectedLndServices(t *testing.T,
	lis *bufconn.Listener) *lndclient.GrpcLndServices {

	t.Helper()

	services, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:        "bufnet",
		Network:           lndclient.NetworkTestnet,
		CustomMacaroonHex: testMacaroonHex,
		Insecure:          true,
		Dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		},
		CheckVersion: LoopMinRequiredLndVersion,
	})
	require.NoError(t, err)

	return services
}

// newTestDaemon creates a loop daemon that uses only temporary directories
// and connects to a swap server address that nothing listens on (the swap
// server connection is established lazily and is not needed for this test).
func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()

	tmpDir := t.TempDir()

	cfg := DefaultConfig()
	cfg.Network = "testnet"
	cfg.LoopDir = tmpDir
	cfg.DataDir = tmpDir
	cfg.LogDir = tmpDir
	cfg.MacaroonPath = filepath.Join(tmpDir, "loop.macaroon")
	cfg.Sqlite = &loopdb.SqliteConfig{
		DatabaseFileName: filepath.Join(tmpDir, "loop.db"),
	}
	cfg.Server = &loopServerConfig{
		Host:  "127.0.0.1:1",
		NoTLS: true,
	}

	return New(&cfg, &ListenerCfg{})
}

// TestSubserverDoesNotCloseSharedLndConn is a regression test for the
// connection ownership bug that caused an embedded loop daemon to close the
// lnd connection that LiT shares among all its subservers. Stopping the
// embedded daemon (either gracefully or after a runtime error) must not
// close the injected lnd connection.
func TestSubserverDoesNotCloseSharedLndConn(t *testing.T) {
	setLogger(btclog.Disabled)

	testCases := []struct {
		name string

		// triggerShutdown initiates the daemon shutdown and returns
		// the error the daemon is expected to report on its main
		// error channel.
		triggerShutdown func(d *Daemon) error
	}{
		{
			name: "graceful stop",
			triggerShutdown: func(d *Daemon) error {
				d.Stop()

				return nil
			},
		},
		{
			name: "runtime error",
			triggerShutdown: func(d *Daemon) error {
				runtimeErr := errors.New("mock runtime error")
				d.internalErrChan <- runtimeErr

				return runtimeErr
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Start the mock lnd node and create the shared lnd
			// connection, exactly like LiT does before starting
			// its subservers.
			lis := startMockLnd(t)
			services := newInjectedLndServices(t, lis)
			conn := services.ClientConn

			// Start the daemon as a subserver with the shared
			// (borrowed) lnd connection.
			d := newTestDaemon(t)
			err := d.StartAsSubserver(services, false)
			require.NoError(t, err)

			// Trigger the shutdown and wait for the daemon to
			// report completion on its main error channel.
			expectedErr := tc.triggerShutdown(d)

			select {
			case err := <-d.ErrChan:
				require.Equal(t, expectedErr, err)

			case <-time.After(testTimeout):
				t.Fatal("timeout waiting for daemon shutdown")
			}

			// The shared lnd connection must still be usable, it
			// must NOT be closed by the subserver.
			require.NotEqual(
				t, connectivity.Shutdown, conn.GetState(),
				"subserver closed the shared lnd connection",
			)

			// The owner of the connection (the test, acting as
			// the parent process) cleans up.
			services.Close()
			require.Equal(
				t, connectivity.Shutdown, conn.GetState(),
			)
		})
	}
}

// TestOwnedLndConnClosedOnStop verifies that the daemon still closes the lnd
// connection when it owns it (standalone mode), i.e. that the ownership
// guard does not leak connections that the daemon created itself.
func TestOwnedLndConnClosedOnStop(t *testing.T) {
	setLogger(btclog.Disabled)

	lis := startMockLnd(t)
	services := newInjectedLndServices(t, lis)
	conn := services.ClientConn

	// Simulate a daemon that created its own lnd connection (standalone
	// mode via Start) by marking the connection as owned.
	d := newTestDaemon(t)
	d.lnd = services
	d.lndOwned = true

	// Stopping the daemon must close the connection it owns.
	d.stop()
	require.Equal(t, connectivity.Shutdown, conn.GetState())
}

// testTLSCert creates an ephemeral self-signed TLS certificate for the mock
// lnd server. The lndclient is configured with Insecure=true, which skips
// certificate verification but still requires the server to speak TLS.
func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: testMockLndName,
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{testMockLndName},
	}

	der, err := x509.CreateCertificate(
		rand.Reader, template, template, &key.PublicKey, key,
	)
	require.NoError(t, err)

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
}
