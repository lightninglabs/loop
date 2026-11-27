package tapsdk

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/stretchr/testify/require"
)

const (
	// loopOutReservationExpiry is the server's reservation refund height.
	loopOutReservationExpiry = 500

	// loopOutHTLCExpiry is the server's HTLC refund height.
	loopOutHTLCExpiry = 450
)

// loopOutFixture is an honest asset Loop Out: a reservation funded by the
// server with scalar three, and the terms under which the client with scalar
// two agreed to receive it through an HTLC.
type loopOutFixture struct {
	terms   HTLCTerms
	hash    [32]byte
	prevout *wire.TxOut
}

// loopOutHTLC builds the Loop Out HTLC, in which the server is the sender that
// refunds and the client is the receiver that claims with the preimage.
func loopOutHTLC(t *testing.T, hash [32]byte, expiry uint32) *swapkit.SwapKit {
	t.Helper()
	clientKey, serverKey := bindingPub(2), bindingPub(3)
	kit, err := swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCCLTV,
		SenderKey: serverKey, ReceiverKey: clientKey,
		SenderInternalKey:   serverKey,
		ReceiverInternalKey: clientKey,
		PaymentHash:         hash, Expiry: expiry,
	})
	require.NoError(t, err)

	return kit
}

// loopOutReservation builds the server-funded reservation contract.
func loopOutReservation(t *testing.T,
	height uint32) *swapkit.ReservationKit {

	t.Helper()
	kit, err := swapkit.NewReservationKit(swapkit.ReservationKitParams{
		ServerKey: bindingPub(3), ClientKey: bindingPub(2),
		RefundHeight: height,
	})
	require.NoError(t, err)

	return kit
}

func newLoopOutFixture(t *testing.T) *loopOutFixture {
	t.Helper()
	reservation := loopOutReservation(t, loopOutReservationExpiry)
	key, _, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	scriptKey, err := btcec.ParsePubKey(key.PubKey[:])
	require.NoError(t, err)
	plan := reservation.AnchorPlan()
	internal, err := btcec.ParsePubKey(plan.InternalKey[:])
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(
		txscript.NewBaseTapLeaf(plan.Leaves[0]),
	)
	require.NoError(t, err)
	funded := bindingProof(t, bindingGenesis(), scriptKey, internal,
		sibling, bindingGenesis().FirstPrevOut, 100, false)
	assets, merkle, err := isolatedRoots(funded)
	require.NoError(t, err)
	point := funded.OutPoint()
	prevout := funded.AnchorTx.TxOut[point.Index]

	preimage := [32]byte{42}
	hash := sha256.Sum256(preimage[:])

	return &loopOutFixture{
		hash: hash, prevout: prevout,
		terms: HTLCTerms{
			Funding: reservation,
			Input: sdk.CustomAnchorAssetOutputSummary{
				LogicalOutputID: "reservation",
				ScriptKey:       key.PubKey,
				ScriptMode:      sdk.CustomAssetScriptExternal,
				AssetRef: sdk.AssetRefFromGroupKey(
					bindingPub(11),
				),
				IssuanceID: sdk.AssetID(funded.Asset.ID()),
				AssetType: sdk.AssetType(
					funded.Asset.Genesis.Type,
				),
				Amount: 1000, AnchorValueSat: prevout.Value,
				TaprootAssetRoot:  assets,
				TaprootMerkleRoot: merkle,
				AnchorOutpoint: sdk.Outpoint{
					Txid: sdk.Hash(funded.AnchorTx.TxHash()),
				},
			},
			InputProof:     bindingRaw(t, funded),
			HTLC:           loopOutHTLC(t, hash, loopOutHTLCExpiry),
			SessionContext: hash[:],
		},
	}
}

// commit builds and commits the HTLC transfer with the real SDK builder, as
// the party that proposes it would.
func (f *loopOutFixture) commit(t *testing.T) []byte {
	t.Helper()
	request, err := HTLCRequest(f.terms, 1_000)
	require.NoError(t, err)
	wallet := sdk.NewWallet(&pathClient{t: t}, sdk.NetworkRegtest)
	built, err := wallet.NewCustomAnchorTxBuilder().Build(
		t.Context(), request,
	)
	require.NoError(t, err)
	pkg, err := built.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)
	raw, err := pkg.MarshalBinary()
	require.NoError(t, err)

	return raw
}

// TestHTLCTransferLoopOut checks the Loop Out direction end to end: a
// server-funded reservation is moved into an HTLC that the client can claim.
// The validated package is a zero-fee pay-to-anchor parent, and its
// cooperative signature is valid for the authenticated reservation output.
func TestHTLCTransferLoopOut(t *testing.T) {
	f := newLoopOutFixture(t)
	pkg, err := ValidateHTLCPackage(f.terms, f.commit(t))
	require.NoError(t, err)

	packet, err := DecodePSBT(pkg.CommittedAnchorPsbt)
	require.NoError(t, err)
	tx := packet.UnsignedTx
	require.EqualValues(t, 3, tx.Version)
	require.Len(t, tx.TxIn, 1)
	require.Len(t, tx.TxOut, 2)
	require.Equal(t, f.prevout.Value, tx.TxOut[0].Value)
	require.Zero(t, tx.TxOut[1].Value)
	require.Equal(t, p2aScript, tx.TxOut[1].PkScript)
	require.Zero(t, pkg.Funding.ActualFeeSat)

	// The HTLC output is exactly the client's own contract.
	spend, err := f.terms.HTLC.SuccessSpend(pkg.Outputs[0].TaprootAssetRoot)
	require.NoError(t, err)
	require.Equal(t, spend.PkScript, tx.TxOut[0].PkScript)

	final := finishFallback(t, pkg)
	require.NoError(t, executeAgainst(t, final, f.prevout))
}

// TestHTLCTransferRejectsForeignTerms checks that a package is only accepted
// by a party whose own terms it matches exactly. Each case validates the
// honest package against terms that differ in one respect, which is the same
// as a peer handing over a package built for terms the party never agreed to.
func TestHTLCTransferRejectsForeignTerms(t *testing.T) {
	f := newLoopOutFixture(t)
	raw := f.commit(t)
	_, err := ValidateHTLCPackage(f.terms, raw)
	require.NoError(t, err)

	otherHash := sha256.Sum256([]byte{1})
	swapped, err := swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCCLTV,
		SenderKey: bindingPub(2), ReceiverKey: bindingPub(3),
		SenderInternalKey:   bindingPub(2),
		ReceiverInternalKey: bindingPub(3),
		PaymentHash:         f.hash, Expiry: loopOutHTLCExpiry,
	})
	require.NoError(t, err)
	relative, err := swapkit.NewSwapKit(swapkit.SwapKitParams{
		Policy:    swapkit.SwapHTLCLegacyCSV,
		SenderKey: bindingPub(3), ReceiverKey: bindingPub(2),
		SenderInternalKey:   bindingPub(3),
		ReceiverInternalKey: bindingPub(2),
		PaymentHash:         f.hash, Expiry: loopOutHTLCExpiry,
	})
	require.NoError(t, err)
	deposit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: bindingPub(3), ServerKey: bindingPub(2),
		CSVExpiry: 100,
	})
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func(*HTLCTerms)
	}{{
		name: "payment hash",
		mutate: func(terms *HTLCTerms) {
			terms.HTLC = loopOutHTLC(
				t, otherHash, loopOutHTLCExpiry,
			)
		},
	}, {
		name: "HTLC expiry",
		mutate: func(terms *HTLCTerms) {
			terms.HTLC = loopOutHTLC(
				t, f.hash, loopOutHTLCExpiry+1,
			)
		},
	}, {
		name:   "HTLC roles",
		mutate: func(terms *HTLCTerms) { terms.HTLC = swapped },
	}, {
		name:   "HTLC refund policy",
		mutate: func(terms *HTLCTerms) { terms.HTLC = relative },
	}, {
		name: "session context",
		mutate: func(terms *HTLCTerms) {
			terms.SessionContext = otherHash[:]
		},
	}, {
		name: "reservation expiry",
		mutate: func(terms *HTLCTerms) {
			terms.Funding = loopOutReservation(
				t, loopOutReservationExpiry+1,
			)
		},
	}, {
		name:   "funding contract",
		mutate: func(terms *HTLCTerms) { terms.Funding = deposit },
	}, {
		name:   "asset amount",
		mutate: func(terms *HTLCTerms) { terms.Input.Amount++ },
	}, {
		name: "anchor value",
		mutate: func(terms *HTLCTerms) {
			terms.Input.AnchorValueSat++
		},
	}, {
		name: "funding outpoint",
		mutate: func(terms *HTLCTerms) {
			terms.Input.AnchorOutpoint.Index++
		},
	}, {
		name:   "missing HTLC",
		mutate: func(terms *HTLCTerms) { terms.HTLC = nil },
	}, {
		name:   "missing funding",
		mutate: func(terms *HTLCTerms) { terms.Funding = nil },
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			terms := f.terms
			test.mutate(&terms)
			_, err := ValidateHTLCPackage(terms, raw)
			require.ErrorIs(t, err, swapkit.ErrInvalid)
		})
	}
}

// TestHTLCRequestRequiresFundedContract checks that a request is only built
// for an output that is really locked by the funding contract, and that the
// HTLC output keeps the requested sweep fee above the dust limit.
func TestHTLCRequestRequiresFundedContract(t *testing.T) {
	f := newLoopOutFixture(t)
	value := uint64(f.prevout.Value)

	_, err := HTLCRequest(f.terms, value-330)
	require.NoError(t, err)
	_, err = HTLCRequest(f.terms, value-329)
	require.ErrorIs(t, err, swapkit.ErrInvalid)

	terms := f.terms
	terms.Funding = loopOutReservation(t, loopOutReservationExpiry+1)
	_, err = HTLCRequest(terms, 1_000)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
