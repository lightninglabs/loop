package hyperloop

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Store is the interface that stores the hyperloop.
type Store interface {
	// CreateHyperloop stores the hyperloop in the database.
	CreateHyperloop(ctx context.Context, hyperloop *HyperLoop) error

	// UpdateHyperloop updates the hyperloop in the database.
	UpdateHyperloop(ctx context.Context, hyperloop *HyperLoop) error

	// CreateHyperloopParticipant stores the hyperloop participant in the
	// database.
	CreateHyperloopParticipant(ctx context.Context,
		participant *HyperLoopParticipant) error

	// UpdateHyperloopParticipant updates the hyperloop participant in the
	// database.
	UpdateHyperloopParticipant(ctx context.Context,
		participant *HyperLoopParticipant) error

	// GetHyperloop retrieves the hyperloop from the database.
	GetHyperloop(ctx context.Context, id ID) (*HyperLoop, error)

	// ListHyperloops lists all existing hyperloops the client has ever
	// made.
	ListHyperloops(ctx context.Context) ([]*HyperLoop, error)
}

type Wallet interface {
	DeriveNextKey(ctx context.Context, family int32) (
		*keychain.KeyDescriptor, error)
}
type Musig2Signer interface {
	MuSig2Sign(ctx context.Context, sessionID [32]byte,
		message [32]byte, cleanup bool) ([]byte, error)

	MuSig2CreateSession(ctx context.Context, version input.MuSig2Version,
		signerLoc *keychain.KeyLocator, signers [][]byte,
		opts ...lndclient.MuSig2SessionOpts) (*input.MuSig2SessionInfo, error)

	MuSig2RegisterNonces(ctx context.Context, sessionID [32]byte,
		nonces [][66]byte) (bool, error)
}
