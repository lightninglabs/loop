package policy

import (
	"bytes"
	"context"
	"fmt"

	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/proof"
)

const (
	// MaxProvenanceDepth bounds how deeply additional input files may nest
	// in an accepted proof history.
	MaxProvenanceDepth = 16

	// MaxProvenanceBytes bounds the total size of the proofs decoded while
	// walking an accepted proof history. Decoding a proof copies every
	// input file nested beneath it, so without a budget the work grows with
	// the nesting depth times the file size rather than with the file size.
	MaxProvenanceBytes = 64 << 20
)

// CheckProofFileSize rejects a proof file larger than MaxProvenanceBytes before
// it is copied or decoded. Proof files share the limit of the decoding budget,
// which is below the maximum file size of Taproot Assets.
func CheckProofFileSize(rawFile []byte) error {
	return checkProofFileSize(rawFile, MaxProvenanceBytes)
}

// checkProofFileSize implements CheckProofFileSize with an explicit limit.
func checkProofFileSize(rawFile []byte, maxBytes int) error {
	if len(rawFile) > maxBytes {
		return fmt.Errorf("proof file of %d bytes exceeds the %d byte "+
			"limit", len(rawFile), maxBytes)
	}

	return proof.CheckMaxFileSize(rawFile)
}

// ActivationHeight returns the block height from which transition proofs on
// the given network must satisfy the Taproot Assets v0.8.5 activation rules.
// A network without a scheduled height, such as regtest, uses tapd's
// process-wide default, which is unset unless configured. The boolean is false
// when no activation rules apply.
func ActivationHeight(params *address.ChainParams) (uint32, bool) {
	height := proof.DefaultActivationHeight()
	if params != nil && params.Params != nil {
		scheduled := proof.NetworkActivationHeight(params.Params.Name)
		if scheduled.IsSome() {
			height = scheduled
		}
	}

	return height.UnwrapOr(0), height.IsSome()
}

// ValidateActivation applies the Taproot Assets v0.8.5 activation rules to a
// proof as tapd applies them to a confirmed proof. Issuance proofs are exempt,
// as is a proof that claims a non-zero block height below the activation
// height. Any other transition proof must be of version one and carry the root
// locator proof of a split, the STXO proofs of a split asset's root, and the
// spender proofs of its transfer root. Only full proof verification
// authenticates the claimed height.
func ValidateActivation(p *proof.Proof, activationHeight uint32) error {
	if p == nil {
		return fmt.Errorf("proof is required")
	}
	if p.Asset.IsGenesisAsset() {
		return nil
	}
	if p.BlockHeight != 0 && p.BlockHeight < activationHeight {
		return nil
	}

	// A split asset proves its transfer root through its split root proof.
	splitAsset := p.Asset.HasSplitCommitmentWitness()
	rootProof := &p.InclusionProof
	if splitAsset {
		rootProof = p.SplitRootProof
	}

	var missing error
	switch {
	case p.Version == proof.TransitionV0:
		missing = proof.ErrTransitionV1Required

	case (splitAsset || p.Asset.SplitCommitmentRoot != nil) &&
		p.RootLocatorProof == nil:

		missing = proof.ErrMissingRootLocatorProof

	case splitAsset && (rootProof == nil ||
		rootProof.CommitmentProof == nil ||
		len(rootProof.CommitmentProof.STXOProofs) == 0):

		missing = proof.ErrMissingSplitSTXOProofs

	case rootProof == nil || rootProof.CommitmentProof == nil ||
		len(rootProof.CommitmentProof.SpenderProofs) == 0:

		missing = proof.ErrMissingSpenderProofs
	}
	if missing != nil {
		return fmt.Errorf("%w: proof at height %d", missing,
			p.BlockHeight)
	}

	return nil
}

// provenanceFile is a proof file awaiting the provenance walk, together with
// its nesting depth below the supplied file.
type provenanceFile struct {
	file  *proof.File
	depth int
}

// ValidateProvenance rejects proof histories that establish no provenance,
// before tapd verifies issuance and transitions. Every file, including each
// nested additional input, must start at genesis and contain no ownership
// challenge, and every proof must satisfy the Taproot Assets v0.8.5
// activation rules of the given network for its claimed height. Nested files
// are walked iteratively so peer input cannot determine the call-stack depth,
// and the nesting depth and total decoded size are bounded so peer input
// cannot amplify the work. These structural checks alone do not establish
// that an asset is valid.
func ValidateProvenance(ctx context.Context, file *proof.File,
	params *address.ChainParams) error {

	return validateProvenance(
		ctx, file, params, MaxProvenanceDepth, MaxProvenanceBytes,
	)
}

// validateProvenance implements ValidateProvenance with explicit bounds.
func validateProvenance(ctx context.Context, file *proof.File,
	params *address.ChainParams, maxDepth, maxBytes int) error {

	if file == nil {
		return fmt.Errorf("proof file is required")
	}
	walk := &provenanceWalk{maxDepth: maxDepth, maxBytes: maxBytes}
	walk.activationHeight, walk.activated = ActivationHeight(params)

	pending := []provenanceFile{{file: file}}
	for len(pending) > 0 {
		last := len(pending) - 1
		entry := pending[last]
		pending[last] = provenanceFile{}
		pending = pending[:last]

		inputs, err := walk.visit(ctx, entry)
		if err != nil {
			return err
		}
		pending = append(pending, inputs...)
	}

	return nil
}

// provenanceWalk holds the bounds and the decoding budget shared by every
// file of one provenance walk.
type provenanceWalk struct {
	maxDepth         int
	maxBytes         int
	decodedBytes     int
	activationHeight uint32
	activated        bool
}

// visit checks every proof of a file and returns its additional inputs.
func (w *provenanceWalk) visit(ctx context.Context,
	entry provenanceFile) ([]provenanceFile, error) {

	file := entry.file
	if file.Version != ProofFileVersion {
		return nil, fmt.Errorf("unsupported proof file version %d",
			file.Version)
	}
	if file.NumProofs() == 0 {
		return nil, fmt.Errorf("proof history is empty")
	}

	var inputs []provenanceFile
	for idx := range file.NumProofs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := w.decode(file, idx)
		if err != nil {
			return nil, err
		}
		if err := w.check(p, idx == 0); err != nil {
			return nil, err
		}

		if len(p.AdditionalInputs) == 0 {
			continue
		}
		if entry.depth >= w.maxDepth {
			return nil, fmt.Errorf("proof inputs nest more than %d "+
				"levels deep", w.maxDepth)
		}
		for inputIdx := range p.AdditionalInputs {
			inputs = append(inputs, provenanceFile{
				file:  &p.AdditionalInputs[inputIdx],
				depth: entry.depth + 1,
			})
		}
	}

	return inputs, nil
}

// decode charges a proof's size to the budget before decoding the proof, as
// decoding copies every input file nested in it.
func (w *provenanceWalk) decode(file *proof.File, idx int) (*proof.Proof,
	error) {

	rawProof, err := file.RawProofAt(uint32(idx))
	if err != nil {
		return nil, fmt.Errorf("unable to read proof: %w", err)
	}
	w.decodedBytes += len(rawProof)
	if w.decodedBytes > w.maxBytes {
		return nil, fmt.Errorf("proof history exceeds the %d byte "+
			"decoding budget", w.maxBytes)
	}

	p := &proof.Proof{}
	if err := p.Decode(bytes.NewReader(rawProof)); err != nil {
		return nil, fmt.Errorf("unable to decode proof: %w", err)
	}

	return p, nil
}

// check applies the provenance and activation rules to one proof, the first
// of its file if first is set.
func (w *provenanceWalk) check(p *proof.Proof, first bool) error {
	if err := ValidateTransitionVersion(p.Version); err != nil {
		return err
	}
	if p.ChallengeWitness != nil {
		return fmt.Errorf("proof history contains an ownership challenge")
	}
	if first && !p.Asset.IsGenesisAsset() {
		return fmt.Errorf("proof history must start at genesis")
	}
	if !w.activated {
		return nil
	}

	return ValidateActivation(p, w.activationHeight)
}
