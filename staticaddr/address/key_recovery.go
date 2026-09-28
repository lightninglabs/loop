package address

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

// reconciledKeyFamilies are the static address key families whose lnd
// counters must stay ahead of Loop's persisted keys after a wallet restore.
// The legacy family also holds static address loop-in HTLC keys.
var reconciledKeyFamilies = []keychain.KeyFamily{
	keychain.KeyFamily(swap.StaticSingleAddressKeyFamily),
	keychain.KeyFamily(swap.StaticMultiAddressKeyFamily),
	keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
}

// staticAddressKeyMaxima retains the highest persisted key in each static
// address family while address records are loaded.
type staticAddressKeyMaxima map[keychain.KeyFamily]*AddressParameters

// observe validates a static address index and updates its family's maximum.
func (lastKeys staticAddressKeyMaxima) observe(params *AddressParameters) error {
	family := params.KeyLocator.Family
	if !slices.Contains(reconciledKeyFamilies, family) {
		return nil
	}
	if params.KeyLocator.Index >= hdkeychain.HardenedKeyStart {
		return fmt.Errorf("invalid static address key index %d",
			params.KeyLocator.Index)
	}
	last := lastKeys[family]
	if last == nil || params.KeyLocator.Index > last.KeyLocator.Index {
		lastKeys[family] = params
	}
	return nil
}

// reconcileKeyIndices advances restored lnd key counters past the maxima
// collected while loading addresses. It runs before publishing the active
// address index, while startup or the issuance gate prevents address creation.
func (m *Manager) reconcileKeyIndices(ctx context.Context,
	lastKeys staticAddressKeyMaxima) error {

	if len(lastKeys) == 0 {
		return nil
	}

	// An unfiltered request includes lnd's internal 1017' accounts. The
	// external key count is the next index, not the last allocated index.
	// These fields are available at our minimum lnd version (v0.18.4).
	accounts, err := m.cfg.WalletKit.ListAccounts(
		ctx, "", walletrpc.AddressType_UNKNOWN,
	)
	if err != nil {
		return fmt.Errorf("list static address key accounts: %w", err)
	}

	for _, family := range reconciledKeyFamilies {
		last := lastKeys[family]
		if last == nil {
			continue
		}
		lastIndex, err := m.lastUsedKeyIndex(ctx, last)
		if err != nil {
			return err
		}
		path := fmt.Sprintf("m/%d'/%d'/%d'", keychain.BIP0043Purpose,
			m.cfg.ChainParams.HDCoinType, family)
		var next uint32
		found := false
		for _, account := range accounts {
			if account.GetDerivationPath() != path {
				continue
			}
			if found {
				return fmt.Errorf("duplicate key account %s", path)
			}
			found = true
			next = account.GetExternalKeyCount()
		}

		// A missing family after a successful listing starts at zero.
		// Already synchronized or advanced wallets need no derivations.
		if next > lastIndex {
			continue
		}
		err = m.advanceKeyIndex(ctx, last, lastIndex, next)
		if err != nil {
			return err
		}
	}

	return nil
}

// lastUsedKeyIndex returns the highest key index Loop has used in the family of
// the given address. Static address loop-in HTLC keys share the legacy family,
// so their highest index also counts for that family.
func (m *Manager) lastUsedKeyIndex(ctx context.Context,
	last *AddressParameters) (uint32, error) {

	family := last.KeyLocator.Family
	lastIndex := last.KeyLocator.Index
	if family != keychain.KeyFamily(swap.StaticSingleAddressKeyFamily) {
		return lastIndex, nil
	}

	htlcIndex, ok, err := m.cfg.Store.GetMaxStaticAddressHtlcKeyIndex(
		ctx, family,
	)
	if err != nil {
		return 0, fmt.Errorf("load static address htlc key index: %w",
			err)
	}
	if !ok {
		return lastIndex, nil
	}
	if htlcIndex >= hdkeychain.HardenedKeyStart {
		return 0, fmt.Errorf("invalid static address htlc key index %d",
			htlcIndex)
	}

	return max(lastIndex, htlcIndex), nil
}

// advanceKeyIndex checks the restored wallet identity against the persisted
// address before consuming any keys, then advances the family's counter past
// lastIndex. Every successful derivation is durable in lnd, so interrupted
// recovery resumes from its updated counter without changing Loop's address
// records.
func (m *Manager) advanceKeyIndex(ctx context.Context,
	last *AddressParameters, lastIndex, next uint32) error {

	key, err := m.cfg.WalletKit.DeriveKey(ctx, &last.KeyLocator)
	if err != nil {
		return fmt.Errorf("verify static address key family %d: %w",
			last.KeyLocator.Family, err)
	}
	if key == nil || key.PubKey == nil || last.ClientPubkey == nil ||
		!key.PubKey.IsEqual(last.ClientPubkey) {

		return fmt.Errorf("restored wallet does not match static "+
			"address key family %d", last.KeyLocator.Family)
	}

	start := next
	target := lastIndex + 1
	lastLog := time.Now()
	var derived uint32
	log.Infof("Reconciling static address key family %d: next=%d, "+
		"target=%d, remaining=%d", last.KeyLocator.Family, next,
		target, target-next)

	for next <= lastIndex {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := m.cfg.WalletKit.DeriveNextKey(
			ctx, int32(last.KeyLocator.Family),
		)
		if err != nil {
			return fmt.Errorf("advance static address key "+
				"family %d: %w", last.KeyLocator.Family, err)
		}
		if key == nil || key.Family != last.KeyLocator.Family ||
			key.Index < next || key.Index >= hdkeychain.HardenedKeyStart {

			return fmt.Errorf("invalid derived key while advancing "+
				"static address family %d", last.KeyLocator.Family)
		}
		next = key.Index + 1
		derived++
		if next < target && (derived%100 == 0 ||
			time.Since(lastLog) >= 5*time.Second) {

			log.Infof("Static address key reconciliation progress: "+
				"family=%d, next=%d, target=%d, remaining=%d, "+
				"progress=%d%%", last.KeyLocator.Family, next,
				target, target-next,
				uint64(next-start)*100/uint64(target-start))
			lastLog = time.Now()
		}
	}

	log.Infof("Reconciled static address key family %d: next=%d",
		last.KeyLocator.Family, next)
	return nil
}
