package deposit

import (
	"context"
	"testing"

	"github.com/lightninglabs/loop/assets/internal/policy"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// supportedTapdClient supplies the current backend version for kit RPC mocks.
// RPC methods not exercised by a test remain supplied by the narrow interface.
type supportedTapdClient struct {
	AddressProofClient
}

func (supportedTapdClient) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: testSupportedTapd}, nil
}

type versionedAddressClient struct {
	*addressClient

	version string
}

func (c *versionedAddressClient) GetInfo(context.Context,
	*taprpc.GetInfoRequest, ...grpc.CallOption) (*taprpc.GetInfoResponse,
	error) {

	return &taprpc.GetInfoResponse{Version: c.version}, nil
}

// TestAddressTapdVersion ensures all address operations reject an unsafe
// running backend before issuing an address that could receive new funds.
func TestAddressTapdVersion(t *testing.T) {
	for _, version := range []string{"0.8.5-alpha", testSupportedTapd} {
		for _, operation := range []string{testDepositAddress, "HTLC", "CLTV HTLC"} {
			t.Run(version+"/"+operation, func(t *testing.T) {
				genesis := testAddressGenesis()
				kit := newAddressKit(t, &genesis, nil)
				client := &versionedAddressClient{
					addressClient: &addressClient{genesis: genesis},
					version:       version,
				}
				var err error
				switch operation {
				case "HTLC":
					_, _, err = kit.NewHtlcAddr(
						t.Context(), client, 1000,
						lntypes.Hash{1}, 144,
					)
				case "CLTV HTLC":
					_, _, err = kit.NewCLTVHtlcAddr(
						t.Context(), client, 1000,
						lntypes.Hash{1}, 1_000_000,
					)

				default:
					_, err = kit.NewAddr(t.Context(), client, 1000)
				}
				if version == testSupportedTapd {
					require.NoError(t, err)
					require.Equal(t, 1, client.calls)
					return
				}

				require.ErrorIs(t, err, policy.ErrTapdVersionUnsupported)
				require.Zero(t, client.calls)
			})
		}
	}
}

type versionedProofVerifier struct {
	*proofVerifierMock

	version string
}

func (v *versionedProofVerifier) GetInfo(context.Context,
	*taprpc.GetInfoRequest, ...grpc.CallOption) (*taprpc.GetInfoResponse,
	error) {

	return &taprpc.GetInfoResponse{Version: v.version}, nil
}

// TestVerifyProofFileTapdVersion requires an upgraded verifier for untrusted
// histories, while preserving local recovery with a previously verified proof.
func TestVerifyProofFileTapdVersion(t *testing.T) {
	for _, version := range []string{"0.8.5-alpha", testSupportedTapd} {
		t.Run(version, func(t *testing.T) {
			f := newWitnessFixture(t)
			verifier := &versionedProofVerifier{
				proofVerifierMock: &proofVerifierMock{
					response: &taprpc.VerifyProofResponse{Valid: true},
				},
				version: version,
			}
			outpoint := f.proof.OutPoint()
			_, err := f.kit.VerifyProofFile(
				t.Context(), verifier, encodeProofFile(t, *f.proof),
				&outpoint, f.proof.Asset.Amount,
			)
			if version == testSupportedTapd {
				require.NoError(t, err)
				require.Equal(t, 1, verifier.calls)
				return
			}

			require.ErrorIs(t, err, policy.ErrTapdVersionUnsupported)
			require.Zero(t, verifier.calls)

			// An older backend cannot accept untrusted history, but it
			// does not prevent locally signing a compliant refund with
			// a proof already verified and retained by the application.
			signer := &localSigner{
				privateKey: f.funderKey, expectedInput: f.assetInIndex,
			}
			_, err = f.kit.CreateTimeoutWitness(
				t.Context(), signer, f.proof, f.packet, f.transfer,
			)
			require.NoError(t, err)
			require.Equal(t, 1, signer.calls)
		})
	}
}
