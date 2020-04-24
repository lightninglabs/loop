package lndclient

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockVersioner struct {
	version *verrpc.Version
	err     error
}

func (m *mockVersioner) GetVersion(_ context.Context) (*verrpc.Version, error) {
	return m.version, m.err
}

// TestCheckVersionCompatibility makes sure the correct error is returned if an
// old lnd is connected that doesn't implement the version RPC, has an older
// version or if an lnd with not all subservers enabled is connected.
func TestCheckVersionCompatibility(t *testing.T) {
	// Make sure a version check against a node that doesn't implement the
	// version RPC always fails.
	unimplemented := &mockVersioner{
		err: status.Error(codes.Unimplemented, "missing"),
	}
	_, err := checkVersionCompatibility(unimplemented, &verrpc.Version{
		AppMajor: 0,
		AppMinor: 10,
		AppPatch: 0,
	})
	if err != ErrVersionCheckNotImplemented {
		t.Fatalf("unexpected error. got '%v' wanted '%v'", err,
			ErrVersionCheckNotImplemented)
	}

	// Next, make sure an older version than what we want is rejected.
	oldVersion := &mockVersioner{
		version: &verrpc.Version{
			AppMajor: 0,
			AppMinor: 10,
			AppPatch: 0,
		},
	}
	_, err = checkVersionCompatibility(oldVersion, &verrpc.Version{
		AppMajor: 0,
		AppMinor: 11,
		AppPatch: 0,
	})
	if err != ErrVersionIncompatible {
		t.Fatalf("unexpected error. got '%v' wanted '%v'", err,
			ErrVersionIncompatible)
	}

	// Finally, make sure we also get the correct error when trying to run
	// against an lnd that doesn't have all required build tags enabled.
	buildTagsMissing := &mockVersioner{
		version: &verrpc.Version{
			AppMajor:  0,
			AppMinor:  10,
			AppPatch:  0,
			BuildTags: []string{"dev", "lntest", "btcd", "signrpc"},
		},
	}
	_, err = checkVersionCompatibility(buildTagsMissing, &verrpc.Version{
		AppMajor:  0,
		AppMinor:  10,
		AppPatch:  0,
		BuildTags: []string{"signrpc", "walletrpc"},
	})
	if err != ErrBuildTagsMissing {
		t.Fatalf("unexpected error. got '%v' wanted '%v'", err,
			ErrVersionIncompatible)
	}
}

// TestLndVersionCheckComparison makes sure the version check comparison works
// correctly and considers all three version levels.
func TestLndVersionCheckComparison(t *testing.T) {
	actual := &verrpc.Version{
		AppMajor: 1,
		AppMinor: 2,
		AppPatch: 3,
	}
	testCases := []struct {
		name        string
		expectMajor uint32
		expectMinor uint32
		expectPatch uint32
		actual      *verrpc.Version
		expectedErr error
	}{
		{
			name:        "no expectation",
			expectMajor: 0,
			expectMinor: 0,
			expectPatch: 0,
			actual:      actual,
			expectedErr: nil,
		},
		{
			name:        "expect exact same version",
			expectMajor: 1,
			expectMinor: 2,
			expectPatch: 3,
			actual:      actual,
			expectedErr: nil,
		},
		{
			name:        "ignore patch if minor is bigger",
			expectMajor: 12,
			expectMinor: 9,
			expectPatch: 14,
			actual: &verrpc.Version{
				AppMajor: 12,
				AppMinor: 22,
				AppPatch: 0,
			},
			expectedErr: nil,
		},
		{
			name:        "all fields different",
			expectMajor: 3,
			expectMinor: 2,
			expectPatch: 1,
			actual:      actual,
			expectedErr: ErrVersionIncompatible,
		},
		{
			name:        "patch version different",
			expectMajor: 1,
			expectMinor: 2,
			expectPatch: 4,
			actual:      actual,
			expectedErr: ErrVersionIncompatible,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := assertVersionCompatible(
				tc.actual, &verrpc.Version{
					AppMajor: tc.expectMajor,
					AppMinor: tc.expectMinor,
					AppPatch: tc.expectPatch,
				},
			)
			if err != tc.expectedErr {
				t.Fatalf("unexpected error, got '%v' wanted "+
					"'%v'", err, tc.expectedErr)
			}
		})
	}
}
