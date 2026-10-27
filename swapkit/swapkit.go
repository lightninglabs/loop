package swapkit

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// PubKey is a compressed secp256k1 key. It has no wallet dependency.
type PubKey = [33]byte

// Hash is a binary 32-byte commitment or payment hash.
type Hash = [32]byte

// AnchorPlan contains the aggregate Bitcoin internal key and policy leaves.
type AnchorPlan struct {
	InternalKey PubKey
	Leaves      [][]byte
}

// SwapHTLCPolicy identifies the immutable Bitcoin contract of an asset HTLC.
type SwapHTLCPolicy uint8

const (
	// SwapHTLCUnspecified forces callers to select a contract explicitly.
	SwapHTLCUnspecified SwapHTLCPolicy = iota

	// SwapHTLCLegacyCSV preserves the original asset deposit HTLC scripts.
	// Expiry is a relative refund delay in blocks.
	SwapHTLCLegacyCSV

	// SwapHTLCStaticLoopInV3 uses Loop's V3 Bitcoin HTLC scripts. Expiry is
	// an absolute refund block height in a separate on-chain contract.
	SwapHTLCStaticLoopInV3
)

const (
	// SwapHTLCCLTV is the direction-neutral name of the same CLTV policy.
	// Sender refunds and receiver claims follow the on-chain asset direction.
	SwapHTLCCLTV = SwapHTLCStaticLoopInV3
)

// SwapKitParams specifies the Bitcoin policy for an asset HTLC. Asset identity,
// amounts, and the asset-level OP_TRUE descriptor are selected separately.
type SwapKitParams struct {
	Policy SwapHTLCPolicy

	// SenderKey authorizes refunds; ReceiverKey authorizes preimage claims.
	// The sender is the client for Loop In and the server for Loop Out.
	SenderKey   PubKey
	ReceiverKey PubKey

	// SenderScriptKey and ReceiverScriptKey are explicit script-role aliases.
	// They may replace SenderKey/ReceiverKey, or equal them when both are set.
	SenderScriptKey   PubKey
	ReceiverScriptKey PubKey

	// Internal keys authorize the cooperative MuSig2 key path. They may
	// equal the corresponding script keys, but must be supplied explicitly.
	SenderInternalKey   PubKey
	ReceiverInternalKey PubKey

	// PaymentHash is SHA256 of the 32-byte success preimage.
	PaymentHash Hash

	// Expiry is a block delay or height, as selected by Policy.
	Expiry uint32
}

// SwapKit constructs an OP_TRUE asset HTLC whose authorization resides on its
// Bitcoin anchor. Its policy is immutable after construction. It does not own
// Lightning invoices, pricing, signing sessions, or preimage release policy.
type SwapKit struct {
	params  SwapKitParams
	anchor  swapAnchorPolicy
	success []byte
	timeout []byte
}

// NewSwapKit validates and constructs an asset HTLC policy.
func NewSwapKit(params SwapKitParams) (*SwapKit, error) {
	if params.SenderScriptKey != (PubKey{}) {
		if params.SenderKey != (PubKey{}) &&
			params.SenderKey != params.SenderScriptKey {

			return nil, fmt.Errorf("conflicting sender script keys")
		}
		params.SenderKey = params.SenderScriptKey
	}
	if params.ReceiverScriptKey != (PubKey{}) {
		if params.ReceiverKey != (PubKey{}) &&
			params.ReceiverKey != params.ReceiverScriptKey {

			return nil, fmt.Errorf("conflicting receiver script keys")
		}
		params.ReceiverKey = params.ReceiverScriptKey
	}
	if err := validateContractKeyRoles(params.SenderInternalKey,
		params.ReceiverInternalKey, params.SenderKey,
		params.ReceiverKey); err != nil {
		return nil, fmt.Errorf("HTLC keys: %w", err)
	}
	if params.PaymentHash == (Hash{}) {
		return nil, fmt.Errorf("payment hash is required")
	}

	var timeoutOp byte
	switch params.Policy {
	case SwapHTLCLegacyCSV:
		if err := validateSwapCSV(params.Expiry); err != nil {
			return nil, err
		}
		if params.Expiry <= 1 {
			return nil, fmt.Errorf("HTLC CSV expiry must exceed the " +
				"one-block success delay")
		}
		timeoutOp = txscript.OP_CHECKSEQUENCEVERIFY

	case SwapHTLCStaticLoopInV3:
		if params.Expiry == 0 ||
			params.Expiry >= txscript.LockTimeThreshold {

			return nil, fmt.Errorf("expiry must be a block height")
		}
		timeoutOp = txscript.OP_CHECKLOCKTIMEVERIFY

	default:
		return nil, fmt.Errorf(
			"unsupported HTLC policy %d", params.Policy,
		)
	}

	success, err := GenSuccessScript(params.ReceiverKey, params.PaymentHash)
	if err != nil {
		return nil, err
	}
	timeout, err := swapTimeoutScript(
		params.SenderKey, params.Expiry, timeoutOp,
	)
	if err != nil {
		return nil, err
	}
	anchor, err := newSwapAnchorPolicy(
		params.SenderInternalKey, params.ReceiverInternalKey,
		[][]byte{success, timeout},
	)
	if err != nil {
		return nil, err
	}

	return &SwapKit{
		params:  params,
		anchor:  anchor,
		success: success,
		timeout: timeout,
	}, nil
}

// Params returns a value copy of the immutable contract parameters.
func (s *SwapKit) Params() SwapKitParams {
	return s.params
}

// AnchorPlan returns the cooperative internal key and Bitcoin HTLC leaves.
func (s *SwapKit) AnchorPlan() AnchorPlan {
	return s.anchor.plan()
}

// CooperativeParticipants returns the exact sorted MuSig2 participant order.
func (s *SwapKit) CooperativeParticipants() []PubKey {
	return slices.Clone(s.anchor.participants)
}

// SuccessScript returns an independent copy of the Bitcoin success leaf.
func (s *SwapKit) SuccessScript() []byte {
	return bytes.Clone(s.success)
}

// TimeoutScript returns an independent copy of the Bitcoin refund leaf.
func (s *SwapKit) TimeoutScript() []byte {
	return bytes.Clone(s.timeout)
}

// SuccessSpend constructs the receiver's Bitcoin script path. assetRoot must
// come from the verified custom-anchor output, not an ownership challenge. The
// spending transaction must have version >= 2 and sequence >= 1 block.
func (s *SwapKit) SuccessSpend(assetRoot Hash) (*SwapAnchorSpendInfo, error) {
	return s.anchor.spend(
		assetRoot, s.success, s.params.ReceiverKey, 1, 0,
	)
}

// TimeoutSpend constructs the sender's Bitcoin refund path. Legacy policy
// requires CSV; StaticLoopInV3 requires nLockTime >= Expiry and a non-final
// input sequence. New CLTV refunds opt into replacement without enabling
// BIP68 relative locks. Maturity must also be checked against the chain.
func (s *SwapKit) TimeoutSpend(assetRoot Hash) (*SwapAnchorSpendInfo, error) {
	sequence, lockTime := s.params.Expiry, uint32(0)
	if s.params.Policy == SwapHTLCStaticLoopInV3 {
		sequence, lockTime = wire.MaxTxInSequenceNum-2, s.params.Expiry
	}

	return s.anchor.spend(
		assetRoot, s.timeout, s.params.SenderKey, sequence, lockTime,
	)
}

// SuccessWitness constructs the Bitcoin success witness after checking the
// preimage. The SDK signing flow verifies the signature against the actual
// transaction; this helper does not sign or release the preimage to a peer.
// ApplyScriptPathWitness accepts only the unlocking prefix: omit the final
// script and control block, which the SDK obtains from its sealed package.
func (s *SwapKit) SuccessWitness(assetRoot Hash, signature []byte,
	preimage [32]byte) ([][]byte, error) {

	if sha256.Sum256(preimage[:]) != s.params.PaymentHash {
		return nil, fmt.Errorf("preimage does not match payment hash")
	}
	spend, err := s.SuccessSpend(assetRoot)
	if err != nil {
		return nil, err
	}
	return spend.witness(signature, preimage[:])
}

// TimeoutWitness constructs the sender's Bitcoin refund witness.
// ApplyScriptPathWitness accepts only the signature, not the final script and
// control block, which the SDK obtains from its sealed package.
func (s *SwapKit) TimeoutWitness(assetRoot Hash,
	signature []byte) ([][]byte, error) {

	spend, err := s.TimeoutSpend(assetRoot)
	if err != nil {
		return nil, err
	}
	return spend.witness(signature, nil)
}

// DepositKitParams specifies a cooperative asset deposit with a unilateral,
// block-based CSV refund to the sender, matching Loop's static address.
type DepositKitParams struct {
	// ClientKey and ServerKey preserve the original two-key constructor.
	// Use these together, or supply all four explicit roles below.
	ClientKey PubKey
	ServerKey PubKey

	SenderInternalKey   PubKey
	ReceiverInternalKey PubKey
	SenderScriptKey     PubKey
	ReceiverScriptKey   PubKey
	CSVExpiry           uint32
}

// DepositKit constructs an asset deposit whose Bitcoin anchor requires both
// participants to spend cooperatively, or the sender after CSV expiry. The
// legacy two-key constructor makes the client the sender.
type DepositKit struct {
	params       DepositKitParams
	anchor       swapAnchorPolicy
	senderScript PubKey
	timeout      []byte
}

// NewDepositKit validates and constructs a static asset deposit policy.
func NewDepositKit(params DepositKitParams) (*DepositKit, error) {
	if err := validateSwapCSV(params.CSVExpiry); err != nil {
		return nil, err
	}
	keys, err := resolveFundingKeys(params.ClientKey, params.ServerKey,
		params.SenderInternalKey, params.ReceiverInternalKey,
		params.SenderScriptKey, params.ReceiverScriptKey)
	if err != nil {
		return nil, fmt.Errorf("deposit keys: %w", err)
	}
	timeout, err := swapTimeoutScript(
		keys.senderScript, params.CSVExpiry,
		txscript.OP_CHECKSEQUENCEVERIFY,
	)
	if err != nil {
		return nil, err
	}
	anchor, err := newSwapAnchorPolicy(
		keys.senderInternal, keys.receiverInternal, [][]byte{timeout},
	)
	if err != nil {
		return nil, err
	}

	return &DepositKit{
		params: params, anchor: anchor,
		senderScript: keys.senderScript, timeout: timeout,
	}, nil
}

// Params returns a value copy of the deposit parameters.
func (d *DepositKit) Params() DepositKitParams {
	return d.params
}

// AnchorPlan returns the cooperative internal key and Bitcoin refund leaf.
func (d *DepositKit) AnchorPlan() AnchorPlan {
	return d.anchor.plan()
}

// CooperativeParticipants returns the exact sorted MuSig2 participant order.
func (d *DepositKit) CooperativeParticipants() []PubKey {
	return slices.Clone(d.anchor.participants)
}

// TimeoutScript returns an independent copy of the Bitcoin refund leaf.
func (d *DepositKit) TimeoutScript() []byte {
	return bytes.Clone(d.timeout)
}

// TimeoutSpend constructs the sender's Bitcoin refund path. The transaction
// needs version >= 2 and sequence >= CSVExpiry blocks. Check chain maturity.
func (d *DepositKit) TimeoutSpend(assetRoot Hash) (
	*SwapAnchorSpendInfo, error) {

	return d.anchor.spend(
		assetRoot, d.timeout, d.senderScript, d.params.CSVExpiry, 0,
	)
}

// TimeoutWitness constructs the deposit's unilateral Bitcoin refund witness.
// ApplyScriptPathWitness accepts only the signature, not the final script and
// control block, which the SDK obtains from its sealed package.
func (d *DepositKit) TimeoutWitness(assetRoot Hash,
	signature []byte) ([][]byte, error) {

	spend, err := d.TimeoutSpend(assetRoot)
	if err != nil {
		return nil, err
	}
	return spend.witness(signature, nil)
}

// ReservationKitParams specifies a server-funded cooperative reservation with
// an absolute block-height refund. It never interprets a CSV delay as a height.
// ServerKey/ClientKey are the two-key convenience form; new callers may instead
// supply all four explicit roles, with the server as sender and refund signer.
type ReservationKitParams struct {
	ServerKey PubKey
	ClientKey PubKey

	SenderInternalKey   PubKey
	ReceiverInternalKey PubKey
	SenderScriptKey     PubKey
	ReceiverScriptKey   PubKey
	RefundHeight        uint32
}

// ReservationKit constructs the server-funded reservation policy. The server
// can refund after CLTV expiry; before that, spending requires cooperation.
type ReservationKit struct {
	params       ReservationKitParams
	anchor       swapAnchorPolicy
	senderScript PubKey
	timeout      []byte
}

// NewReservationKit validates and constructs a CLTV asset reservation.
func NewReservationKit(params ReservationKitParams) (*ReservationKit, error) {
	if params.RefundHeight == 0 ||
		params.RefundHeight >= txscript.LockTimeThreshold {

		return nil, fmt.Errorf("reservation refund must be a block height")
	}
	keys, err := resolveFundingKeys(params.ServerKey, params.ClientKey,
		params.SenderInternalKey, params.ReceiverInternalKey,
		params.SenderScriptKey, params.ReceiverScriptKey)
	if err != nil {
		return nil, fmt.Errorf("reservation keys: %w", err)
	}
	timeout, err := swapTimeoutScript(keys.senderScript,
		params.RefundHeight, txscript.OP_CHECKLOCKTIMEVERIFY)
	if err != nil {
		return nil, err
	}
	anchor, err := newSwapAnchorPolicy(keys.senderInternal,
		keys.receiverInternal, [][]byte{timeout})
	if err != nil {
		return nil, err
	}
	return &ReservationKit{params: params, anchor: anchor,
		senderScript: keys.senderScript, timeout: timeout}, nil
}

// Params returns a value copy of the immutable reservation parameters.
func (r *ReservationKit) Params() ReservationKitParams { return r.params }

// AnchorPlan returns the cooperative internal key and reservation refund leaf.
func (r *ReservationKit) AnchorPlan() AnchorPlan { return r.anchor.plan() }

// CooperativeParticipants returns the exact sorted MuSig2 participant order.
func (r *ReservationKit) CooperativeParticipants() []PubKey {
	return slices.Clone(r.anchor.participants)
}

// TimeoutScript returns an independent copy of the server's CLTV refund leaf.
func (r *ReservationKit) TimeoutScript() []byte { return bytes.Clone(r.timeout) }

// TimeoutSpend constructs the server's refund path. Apply the height locktime
// and non-final input sequence before signing; the template opts into
// replacement without enabling BIP68. Check maturity against the chain.
func (r *ReservationKit) TimeoutSpend(assetRoot Hash) (*SwapAnchorSpendInfo, error) {
	return r.anchor.spend(assetRoot, r.timeout, r.senderScript,
		wire.MaxTxInSequenceNum-2, r.params.RefundHeight)
}

// TimeoutWitness constructs the complete reservation refund witness. The SDK
// signing API instead takes only the signature prefix and adds script/control.
func (r *ReservationKit) TimeoutWitness(assetRoot Hash,
	signature []byte) ([][]byte, error) {

	spend, err := r.TimeoutSpend(assetRoot)
	if err != nil {
		return nil, err
	}
	return spend.witness(signature, nil)
}

// SwapAnchorSpendInfo contains Loop-owned Bitcoin script-path material. Compare
// PkScript with the authenticated prevout before signing. It does not prove
// issuance, provenance, amount, confirmation, or timelock maturity.
type SwapAnchorSpendInfo struct {
	InternalKey       PubKey
	TaprootMerkleRoot Hash
	PkScript          []byte
	LeafScript        []byte
	ControlBlock      []byte
	RequiredSigner    [32]byte
	Sequence          uint32
	LockTime          uint32
}

func (s *SwapAnchorSpendInfo) witness(signature,
	preimage []byte) ([][]byte, error) {

	if err := validateSwapSignature(signature); err != nil {
		return nil, err
	}
	witness := make([][]byte, 0, 4)
	if preimage != nil {
		witness = append(witness, bytes.Clone(preimage))
	}
	return append(
		witness, bytes.Clone(signature), bytes.Clone(s.LeafScript),
		bytes.Clone(s.ControlBlock),
	), nil
}

type swapAnchorPolicy struct {
	internalKey  PubKey
	participants []PubKey
	leaves       [][]byte
}

func newSwapAnchorPolicy(first, second PubKey,
	leaves [][]byte) (swapAnchorPolicy, error) {

	if err := validateSwapKeyPair(first, second); err != nil {
		return swapAnchorPolicy{}, fmt.Errorf("MuSig2 keys: %w", err)
	}
	participants := []PubKey{first, second}
	slices.SortFunc(participants, func(first, second PubKey) int {
		return bytes.Compare(first[:], second[:])
	})
	keys := make([]*btcec.PublicKey, len(participants))
	for i := range participants {
		keys[i], _ = btcec.ParsePubKey(participants[i][:])
	}
	aggregate, _, _, err := musig2.AggregateKeys(keys, false)
	if err != nil {
		return swapAnchorPolicy{}, fmt.Errorf("aggregate keys: %w", err)
	}
	key, err := parsePubKey(aggregate.PreTweakedKey.SerializeCompressed())
	if err != nil {
		return swapAnchorPolicy{}, err
	}
	return swapAnchorPolicy{
		internalKey: key, participants: participants, leaves: leaves,
	}, nil
}

func (p swapAnchorPolicy) plan() AnchorPlan {
	leaves := make([][]byte, len(p.leaves))
	for i := range p.leaves {
		leaves[i] = bytes.Clone(p.leaves[i])
	}
	return AnchorPlan{InternalKey: p.internalKey, Leaves: leaves}
}

func (p swapAnchorPolicy) spend(assetRoot Hash, script []byte, signer PubKey,
	sequence, lockTime uint32) (*SwapAnchorSpendInfo, error) {

	if assetRoot == (Hash{}) {
		return nil, fmt.Errorf("asset commitment root is required")
	}
	leaves := make([]txscript.TapLeaf, len(p.leaves))
	for i := range p.leaves {
		leaves[i] = txscript.NewBaseTapLeaf(p.leaves[i])
	}
	tree := txscript.AssembleTaprootScriptTree(leaves...)
	leafHash := txscript.NewBaseTapLeaf(script).TapHash()
	proof := tree.LeafMerkleProofs[tree.LeafProofIndex[leafHash]]
	internalKey, err := btcec.ParsePubKey(p.internalKey[:])
	if err != nil {
		return nil, err
	}
	control := proof.ToControlBlock(internalKey)
	control.InclusionProof = append(
		bytes.Clone(control.InclusionProof), assetRoot[:]...,
	)
	root := control.RootHash(script)
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, root)
	control.OutputKeyYIsOdd = outputKey.SerializeCompressed()[0] == 0x03
	controlBytes, err := control.ToBytes()
	if err != nil {
		return nil, err
	}
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, err
	}
	return &SwapAnchorSpendInfo{
		InternalKey:       p.internalKey,
		TaprootMerkleRoot: Hash(root),
		PkScript:          pkScript,
		LeafScript:        bytes.Clone(script),
		ControlBlock:      controlBytes,
		RequiredSigner:    [32]byte(signer[1:]),
		Sequence:          sequence,
		LockTime:          lockTime,
	}, nil
}

func swapTimeoutScript(key PubKey, expiry uint32, opcode byte) ([]byte, error) {
	return timeoutScript([32]byte(key[1:]), int64(expiry), opcode)
}

func validateSwapCSV(expiry uint32) error {
	if expiry == 0 || expiry > wire.SequenceLockTimeMask {
		return fmt.Errorf("CSV expiry must be 1 to 65535 blocks")
	}
	return nil
}

func validateSwapKeyPair(first, second PubKey) error {
	if err := validatePubKey("first key", first); err != nil {
		return err
	}
	if err := validatePubKey("second key", second); err != nil {
		return err
	}
	if bytes.Equal(first[1:], second[1:]) {
		return fmt.Errorf("participant keys must differ")
	}
	return nil
}

func validateSwapSignature(signature []byte) error {
	if len(signature) != schnorr.SignatureSize &&
		len(signature) != schnorr.SignatureSize+1 {

		return fmt.Errorf("signature must be 64 or 65 bytes")
	}
	if len(signature) == schnorr.SignatureSize+1 {
		switch txscript.SigHashType(signature[schnorr.SignatureSize]) {
		case txscript.SigHashAll, txscript.SigHashNone,
			txscript.SigHashSingle,
			txscript.SigHashAll | txscript.SigHashAnyOneCanPay,
			txscript.SigHashNone | txscript.SigHashAnyOneCanPay,
			txscript.SigHashSingle | txscript.SigHashAnyOneCanPay:

		default:
			return fmt.Errorf("invalid signature hash type")
		}
	}
	_, err := schnorr.ParseSignature(signature[:schnorr.SignatureSize])
	if err != nil {
		return fmt.Errorf("invalid Schnorr signature: %w", err)
	}
	return nil
}

func parsePubKey(encoded []byte) (PubKey, error) {
	if len(encoded) != 33 {
		return PubKey{}, fmt.Errorf("public key must be 33 bytes")
	}
	if _, err := btcec.ParsePubKey(encoded); err != nil {
		return PubKey{}, err
	}
	return PubKey(encoded), nil
}

func validatePubKey(name string, key PubKey) error {
	if _, err := parsePubKey(key[:]); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// GenSuccessScript constructs the shared Bitcoin HTLC success leaf.
func GenSuccessScript(receiver PubKey, paymentHash Hash) ([]byte, error) {
	if err := validatePubKey("receiver key", receiver); err != nil {
		return nil, err
	}
	if paymentHash == (Hash{}) {
		return nil, fmt.Errorf("payment hash is required")
	}
	return HTLCSuccessScript([32]byte(receiver[1:]), paymentHash)
}

// GenCSVTimeoutScript constructs the shared block-based Bitcoin refund leaf.
func GenCSVTimeoutScript(sender PubKey, expiry uint32) ([]byte, error) {
	if err := validatePubKey("sender key", sender); err != nil {
		return nil, err
	}
	if err := validateSwapCSV(expiry); err != nil {
		return nil, err
	}
	return swapTimeoutScript(
		sender, expiry, txscript.OP_CHECKSEQUENCEVERIFY,
	)
}

type fundingKeys struct {
	senderInternal, receiverInternal PubKey
	senderScript, receiverScript     PubKey
}

func resolveFundingKeys(legacySender, legacyReceiver, senderInternal,
	receiverInternal, senderScript, receiverScript PubKey) (fundingKeys, error) {

	explicit := senderInternal != (PubKey{}) || receiverInternal != (PubKey{}) ||
		senderScript != (PubKey{}) || receiverScript != (PubKey{})
	if explicit {
		if legacySender != (PubKey{}) || legacyReceiver != (PubKey{}) {
			return fundingKeys{}, fmt.Errorf("cannot mix legacy and explicit key roles")
		}
	} else {
		senderInternal, senderScript = legacySender, legacySender
		receiverInternal, receiverScript = legacyReceiver, legacyReceiver
	}
	if err := validateContractKeyRoles(senderInternal, receiverInternal,
		senderScript, receiverScript); err != nil {
		return fundingKeys{}, err
	}
	return fundingKeys{senderInternal: senderInternal,
		receiverInternal: receiverInternal, senderScript: senderScript,
		receiverScript: receiverScript}, nil
}

func validateContractKeyRoles(senderInternal, receiverInternal,
	senderScript, receiverScript PubKey) error {

	for _, sender := range []PubKey{senderInternal, senderScript} {
		for _, receiver := range []PubKey{receiverInternal, receiverScript} {
			if err := validateSwapKeyPair(sender, receiver); err != nil {
				return err
			}
		}
	}
	return nil
}
