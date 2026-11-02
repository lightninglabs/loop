package rest

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
)

const queryTrue = "true"

// walletClient implements tapsdk.WalletClient over REST.
type walletClient struct {
	transport *transport
}

func newWalletClient(tp *transport) *walletClient {
	return &walletClient{transport: tp}
}

// GetInfo returns general information about the tapd instance.
func (w *walletClient) GetInfo(
	ctx context.Context) (*tapsdk.Info, error) {

	var resp jsonGetInfoResponse
	err := w.transport.doGet(
		ctx, "/v1/taproot-assets/getinfo",
		macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalInfo(&resp)
}

// ListAssetRecords lists wallet asset records with optional filtering.
func (w *walletClient) ListAssetRecords(ctx context.Context,
	req *tapsdk.ListAssetsRequest) ([]*tapsdk.AssetRecord, error) {

	params, assetRefFilter, err := listAssetRecordsQueryParams(req)
	if err != nil {
		return nil, err
	}

	path := "/v1/taproot-assets/assets"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp jsonListAssetsResponse
	err = w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	assets := make([]*tapsdk.AssetRecord, 0, len(resp.Assets))
	for _, jsonAsset := range resp.Assets {
		asset, err := unmarshalAsset(jsonAsset)
		if err != nil {
			return nil, err
		}

		if assetRefFilter != nil &&
			!assetRecordMatchesRef(asset, *assetRefFilter) {

			continue
		}

		assets = append(assets, asset)
	}

	return assets, nil
}

func listAssetRecordsQueryParams(
	req *tapsdk.ListAssetsRequest) (url.Values, *tapsdk.AssetRef, error) {

	params := url.Values{}
	var assetRefFilter *tapsdk.AssetRef
	if req == nil {
		return params, assetRefFilter, nil
	}

	if req.WithWitness {
		params.Set("with_witness", queryTrue)
	}
	if req.IncludeSpent {
		params.Set("include_spent", queryTrue)
	}
	if req.IncludeLeased {
		params.Set("include_leased", queryTrue)
	}
	if req.IncludeUnconfirmedMints {
		params.Set("include_unconfirmed_mints", queryTrue)
	}
	if req.MinAmount != 0 {
		params.Set("min_amount", strconv.FormatUint(req.MinAmount, 10))
	}
	if req.MaxAmount != 0 {
		params.Set("max_amount", strconv.FormatUint(req.MaxAmount, 10))
	}

	if req.AssetRef != nil {
		if err := req.AssetRef.Validate(); err != nil {
			return nil, nil, err
		}

		assetRefFilter = req.AssetRef
		if groupKey, ok := req.AssetRef.GroupKey(); ok {
			// grpc-gateway rejects hex for `bytes` query params; it
			// expects URL-safe base64.
			params.Set(
				"group_key",
				base64.URLEncoding.EncodeToString(groupKey[:]),
			)
		}
	}

	if req.AnchorOutpoint != nil {
		params.Set(
			"anchor_outpoint.txid",
			base64.URLEncoding.EncodeToString(
				req.AnchorOutpoint.Txid[:],
			),
		)
		params.Set(
			"anchor_outpoint.output_index",
			strconv.FormatUint(uint64(req.AnchorOutpoint.Index), 10),
		)
	}

	if err := marshalScriptKeyTypeQueryParams(
		params, req.ScriptKeyType,
	); err != nil {
		return nil, nil, err
	}

	return params, assetRefFilter, nil
}

func marshalScriptKeyTypeQueryParams(params url.Values,
	query *tapsdk.ScriptKeyTypeQuery) error {

	if err := query.Validate(); err != nil {
		return err
	}

	if query == nil {
		return nil
	}

	if query.ExplicitType != nil {
		params.Set(
			"script_key_type.explicit_type",
			marshalScriptKeyType(*query.ExplicitType),
		)
		return nil
	}

	if query.AllTypes {
		params.Set("script_key_type.all_types", queryTrue)
	}

	return nil
}

func marshalScriptKeyType(scriptKeyType tapsdk.ScriptKeyType) string {
	switch scriptKeyType {
	case tapsdk.ScriptKeyTypeUnknown:
		return "SCRIPT_KEY_UNKNOWN"
	case tapsdk.ScriptKeyTypeBIP86:
		return "SCRIPT_KEY_BIP86"
	case tapsdk.ScriptKeyTypeScriptPathExternal:
		return "SCRIPT_KEY_SCRIPT_PATH_EXTERNAL"
	case tapsdk.ScriptKeyTypeBurn:
		return "SCRIPT_KEY_BURN"
	case tapsdk.ScriptKeyTypeTombstone:
		return "SCRIPT_KEY_TOMBSTONE"
	case tapsdk.ScriptKeyTypeChannel:
		return "SCRIPT_KEY_CHANNEL"
	case tapsdk.ScriptKeyTypeUniquePedersen:
		return "SCRIPT_KEY_UNIQUE_PEDERSEN"
	default:
		return ""
	}
}

func assetRecordMatchesRef(asset *tapsdk.AssetRecord,
	ref tapsdk.AssetRef) bool {

	if asset == nil {
		return false
	}

	if asset.AssetRef.Equivalent(ref) {
		return true
	}

	assetID, ok := ref.AssetID()
	return ok && asset.Genesis.IssuanceID == assetID
}

// ListBalances returns asset balances keyed by AssetRef.
func (w *walletClient) ListBalances(ctx context.Context,
	req *tapsdk.ListBalancesRequest) (
	*tapsdk.ListBalancesResponse, error) {

	// Group refs must round-trip through tapd's group-by-group_key
	// mode — the server only honors group_key_filter in that mode,
	// and a wallet that learned about the group through universe
	// bootstrap has no per-asset-id rows yet.
	if req != nil && req.AssetRef != nil && req.AssetRef.IsGroupRef() {
		return w.listGroupBalance(ctx, req)
	}

	// Request per-asset-id balances so we get genesis info and
	// group keys. The SDK re-aggregates entries with the same
	// group key into AssetRef-keyed balances.
	params := url.Values{
		"asset_id": {queryTrue},
	}
	if req != nil {
		if req.IncludeLeased {
			params.Set("include_leased", queryTrue)
		}

		if req.AssetRef != nil {
			if assetID, ok := req.AssetRef.AssetID(); ok {
				// grpc-gateway wants URL-safe base64 for
				// `bytes` query params.
				params.Set(
					"asset_filter",
					base64.URLEncoding.EncodeToString(
						assetID[:],
					),
				)
			}
		}
	}

	path := "/v1/taproot-assets/assets/balance?" +
		params.Encode()

	var resp jsonListBalancesResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	unconfirmed, err := parseUint64(resp.UnconfirmedTransfers)
	if err != nil {
		return nil, fmt.Errorf("unconfirmed_transfers: %w", err)
	}

	result := &tapsdk.ListBalancesResponse{
		Balances: make(
			map[string]*tapsdk.Balance,
		),
		UnconfirmedTransfers: unconfirmed,
	}

	for _, ab := range resp.AssetBalances {
		var genesis *tapsdk.IssuanceGenesis
		if ab.AssetGenesis != nil {
			genesis, err = unmarshalIssuanceGenesis(
				ab.AssetGenesis,
			)
			if err != nil {
				return nil, fmt.Errorf("balance "+
					"genesis: %w", err)
			}
		}

		var ref tapsdk.AssetRef

		if ab.GroupKey != "" &&
			(genesis == nil ||
				genesis.Type != tapsdk.AssetTypeCollectible) {

			gkBytes, err := parseHexBytes(
				ab.GroupKey,
			)
			if err != nil {
				return nil, fmt.Errorf("balance "+
					"group key: %w", err)
			}

			gk, err := tapsdk.ParsePubKey(gkBytes)
			if err != nil {
				return nil, fmt.Errorf("balance "+
					"group key: %w", err)
			}

			ref = tapsdk.AssetRefFromGroupKey(gk)
		} else {
			if genesis == nil {
				return nil, fmt.Errorf("balance genesis: " +
					"missing asset genesis")
			}

			ref = tapsdk.AssetRefFromAssetID(
				genesis.IssuanceID,
			)
		}

		bal, err := parseUint64(ab.Balance)
		if err != nil {
			return nil, fmt.Errorf("balance "+
				"amount: %w", err)
		}

		key := ref.String()
		balance, ok := result.Balances[key]
		if !ok {
			balance = &tapsdk.Balance{
				AssetRef: ref,
			}

			result.Balances[key] = balance
		}

		balance.Balance += bal
	}

	return result, nil
}

// listGroupBalance queries a single-group balance via tapd's group-by-group_key
// mode.
func (w *walletClient) listGroupBalance(ctx context.Context,
	req *tapsdk.ListBalancesRequest) (*tapsdk.ListBalancesResponse,
	error) {

	groupKey, ok := req.AssetRef.GroupKey()
	if !ok {
		return nil, fmt.Errorf("group balance requires a group asset ref")
	}

	// grpc-gateway decodes query-string `bytes` fields as URL-safe
	// base64 (padding optional). Hex is rejected.
	params := url.Values{
		"group_key": {queryTrue},
		"group_key_filter": {
			base64.URLEncoding.EncodeToString(groupKey[:]),
		},
	}
	if req.IncludeLeased {
		params.Set("include_leased", queryTrue)
	}

	path := "/v1/taproot-assets/assets/balance?" + params.Encode()

	var resp jsonListBalancesResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	unconfirmed, err := parseUint64(resp.UnconfirmedTransfers)
	if err != nil {
		return nil, fmt.Errorf("unconfirmed_transfers: %w", err)
	}

	result := &tapsdk.ListBalancesResponse{
		Balances:             map[string]*tapsdk.Balance{},
		UnconfirmedTransfers: unconfirmed,
	}

	groupBalance, ok := resp.AssetGroupBalances[hex.EncodeToString(groupKey[:])]
	if !ok || groupBalance == nil {
		return result, nil
	}

	amount, err := parseUint64(groupBalance.Balance)
	if err != nil {
		return nil, fmt.Errorf("balance amount: %w", err)
	}
	if amount == 0 {
		return result, nil
	}

	result.Balances[req.AssetRef.String()] = &tapsdk.Balance{
		AssetRef: *req.AssetRef,
		Balance:  amount,
	}

	return result, nil
}

// ListTransfers lists outgoing transfers with optional filtering.
func (w *walletClient) ListTransfers(ctx context.Context,
	req *tapsdk.ListTransfersRequest) (
	[]*tapsdk.AssetTransfer, error) {

	params := url.Values{}
	if req != nil && req.AnchorTxid != "" {
		params.Set("anchor_txid", req.AnchorTxid)
	}

	path := "/v1/taproot-assets/assets/transfers"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp jsonListTransfersResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	transfers := make(
		[]*tapsdk.AssetTransfer, 0, len(resp.Transfers),
	)
	for _, jsonTransfer := range resp.Transfers {
		transfer, err := unmarshalAssetTransfer(jsonTransfer)
		if err != nil {
			return nil, err
		}

		transfers = append(transfers, transfer)
	}

	return transfers, nil
}

// jsonSendAssetRequest is the JSON body for the SendAsset
// RPC.
type jsonSendAssetRequest struct {
	TapAddrs []string `json:"tap_addrs,omitempty"`
	FeeRate  uint32   `json:"fee_rate,omitempty"`
	Label    string   `json:"label,omitempty"`

	Recipients           []*jsonAddressWithAmount `json:"addresses_with_amounts,omitempty"`        //nolint:lll
	SkipProofCourierPing bool                     `json:"skip_proof_courier_ping_check,omitempty"` //nolint:lll
}

// jsonAddressWithAmount is the JSON shape of taprpc.AddressWithAmount.
type jsonAddressWithAmount struct {
	TapAddr string `json:"tap_addr"`
	Amount  string `json:"amount"`
}

// SendAsset performs a one-shot address-based send.
func (w *walletClient) SendAsset(ctx context.Context,
	req *tapsdk.SendAssetRequest) (
	*tapsdk.AssetTransfer, error) {

	body := &jsonSendAssetRequest{}
	if req != nil {
		feeRate, err := feeRateSatPerKWeight(req.FeeRate)
		if err != nil {
			return nil, err
		}

		body.FeeRate = feeRate
		body.Label = req.Label
		body.SkipProofCourierPing = req.SkipProofCourierPingCheck

		// tapd's REST surface mirrors the gRPC one: TapAddrs for
		// addresses that encode their own amount, AddressesWithAmounts
		// for the explicit-amount path. The two fields are mutually
		// exclusive; mixed inputs are a caller contract violation that
		// Wallet.SendMulti normalises.
		if err := marshalSendRecipients(req.Recipients, body); err != nil {
			return nil, err
		}
	}

	var resp jsonSendAssetResponse
	err := w.transport.doPost(
		ctx, "/v1/taproot-assets/send",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalAssetTransfer(resp.Transfer)
}

// marshalSendRecipients writes Recipients onto the JSON body, picking
// the embedded-amount path (TapAddrs) when every recipient uses its address
// amount and the explicit-amount path (AddressesWithAmounts) when every
// recipient has an explicit amount. Mixed inputs violate the low-level contract
// and produce tapsdk.ErrMixedRecipientAmounts.
func marshalSendRecipients(recipients []tapsdk.Recipient,
	body *jsonSendAssetRequest) error {

	if len(recipients) == 0 {
		return nil
	}

	allEmbedded, anyEmbedded := true, false
	for _, r := range recipients {
		_, hasAmount := r.Amount()
		if !hasAmount {
			anyEmbedded = true
		} else {
			allEmbedded = false
		}
	}
	if !allEmbedded && anyEmbedded {
		return tapsdk.ErrMixedRecipientAmounts
	}

	if allEmbedded {
		body.TapAddrs = make([]string, len(recipients))
		for i, r := range recipients {
			body.TapAddrs[i] = r.Address
		}
		return nil
	}

	body.Recipients = make(
		[]*jsonAddressWithAmount, 0, len(recipients),
	)
	for _, r := range recipients {
		amount, _ := r.Amount()
		body.Recipients = append(body.Recipients,
			&jsonAddressWithAmount{
				TapAddr: r.Address,
				Amount:  fmt.Sprintf("%d", amount),
			},
		)
	}

	return nil
}

// jsonNewAddrRequest is the JSON body for the NewAddr RPC.
type jsonNewAddrRequest struct {
	AssetID  string `json:"asset_id,omitempty"`
	Amount   string `json:"amt,omitempty"`
	GroupKey string `json:"group_key,omitempty"`

	ScriptKey   *jsonScriptKey     `json:"script_key,omitempty"`
	InternalKey *jsonKeyDescriptor `json:"internal_key,omitempty"`

	TapscriptSibling          string `json:"tapscript_sibling,omitempty"`             //nolint:lll
	ProofCourierAddr          string `json:"proof_courier_addr,omitempty"`            //nolint:lll
	SkipProofCourierConnCheck bool   `json:"skip_proof_courier_conn_check,omitempty"` //nolint:lll

	AssetVersion   string `json:"asset_version,omitempty"`
	AddressVersion string `json:"address_version,omitempty"`
}

// NewAddr creates a new Taproot Asset address.
func (w *walletClient) NewAddr(ctx context.Context,
	req *tapsdk.NewAddressRequest) (*tapsdk.Address, error) {

	body := &jsonNewAddrRequest{}
	if req != nil {
		if !req.AssetRef.IsZero() {
			if err := req.AssetRef.Validate(); err != nil {
				return nil, err
			}
		}
		if assetID, ok := req.AssetRef.AssetID(); ok {
			body.AssetID = hex.EncodeToString(assetID[:])
		}
		if req.Amount > 0 {
			body.Amount = fmt.Sprintf("%d", req.Amount)
		}
		if groupKey, ok := req.AssetRef.GroupKey(); ok {
			body.GroupKey = hex.EncodeToString(groupKey[:])
		}
		if req.TapscriptSibling != nil {
			body.TapscriptSibling = hex.EncodeToString(
				req.TapscriptSibling,
			)
		}
		body.ProofCourierAddr = req.ProofCourierAddr
		body.SkipProofCourierConnCheck = req.SkipProofCourierConnCheck

		if req.AssetVersion != nil {
			body.AssetVersion = marshalAssetVersionJSON(
				*req.AssetVersion,
			)
		}
		if req.AddressVersion != nil {
			body.AddressVersion = marshalAddressVersionJSON(
				*req.AddressVersion,
			)
		}

		if req.ScriptKey != nil {
			body.ScriptKey = marshalScriptKeyJSON(req.ScriptKey)
		}
		if req.InternalKey != nil {
			body.InternalKey = marshalKeyDescriptorJSON(
				req.InternalKey,
			)
		}
	}

	var resp jsonAddr
	err := w.transport.doPost(
		ctx, "/v1/taproot-assets/addrs",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalAddr(&resp)
}

// DecodeAddr decodes a bech32m Taproot Asset address string.
func (w *walletClient) DecodeAddr(ctx context.Context,
	addr string) (*tapsdk.Address, error) {

	body := map[string]string{"addr": addr}

	var resp jsonAddr
	err := w.transport.doPost(
		ctx, "/v1/taproot-assets/addrs/decode",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalAddr(&resp)
}

// QueryAddrs returns addresses previously created by this tapd
// instance.
func (w *walletClient) QueryAddrs(ctx context.Context,
	query *tapsdk.AddressQuery) ([]*tapsdk.Address, error) {

	params := url.Values{}
	if query != nil {
		if query.CreatedAfter > 0 {
			params.Set("created_after", fmt.Sprintf(
				"%d", query.CreatedAfter,
			))
		}
		if query.CreatedBefore > 0 {
			params.Set("created_before", fmt.Sprintf(
				"%d", query.CreatedBefore,
			))
		}
		if query.Limit > 0 {
			params.Set("limit", fmt.Sprintf(
				"%d", query.Limit,
			))
		}
		if query.Offset > 0 {
			params.Set("offset", fmt.Sprintf(
				"%d", query.Offset,
			))
		}
	}

	path := "/v1/taproot-assets/addrs"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp jsonQueryAddrsResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	addrs := make([]*tapsdk.Address, 0, len(resp.Addrs))
	for _, jsonAddr := range resp.Addrs {
		addr, err := unmarshalAddr(jsonAddr)
		if err != nil {
			return nil, err
		}

		addrs = append(addrs, addr)
	}

	return addrs, nil
}

// jsonAddrReceivesRequest is the JSON body for the AddrReceives RPC.
type jsonAddrReceivesRequest struct {
	FilterAddr     string `json:"filter_addr,omitempty"`
	FilterStatus   string `json:"filter_status,omitempty"`
	StartTimestamp uint64 `json:"start_timestamp,omitempty"`
	EndTimestamp   uint64 `json:"end_timestamp,omitempty"`
	Offset         int32  `json:"offset,omitempty"`
	Limit          int32  `json:"limit,omitempty"`
	Direction      string `json:"direction,omitempty"`
}

// AddrReceives returns incoming transfer events for addresses.
func (w *walletClient) AddrReceives(ctx context.Context,
	query *tapsdk.AddressReceivesQuery) (
	[]*tapsdk.AddressEvent, error) {

	body := &jsonAddrReceivesRequest{}
	if query != nil {
		body.FilterAddr = query.FilterAddr
		body.FilterStatus = marshalAddrEventStatusJSON(
			query.FilterStatus,
		)
		body.StartTimestamp = query.StartTimestamp
		body.EndTimestamp = query.EndTimestamp
		body.Offset = query.Offset
		body.Limit = query.Limit
	}

	var resp jsonAddrReceivesResponse
	err := w.transport.doPost(
		ctx, "/v1/taproot-assets/addrs/receives",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	events := make([]*tapsdk.AddressEvent, 0, len(resp.Events))
	for _, jsonEvent := range resp.Events {
		event, err := unmarshalAddrEvent(jsonEvent)
		if err != nil {
			return nil, err
		}

		events = append(events, event)
	}

	return events, nil
}

// ListUtxos lists managed UTXOs with optional filtering.
func (w *walletClient) ListUtxos(ctx context.Context,
	req *tapsdk.ListUtxosRequest) (
	map[string]*tapsdk.ManagedUtxo, error) {

	params := url.Values{}
	if req != nil && req.IncludeLeased {
		params.Set("include_leased", queryTrue)
	}

	path := "/v1/taproot-assets/assets/utxos"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp jsonListUtxosResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(
		map[string]*tapsdk.ManagedUtxo, len(resp.ManagedUtxos),
	)
	for k, v := range resp.ManagedUtxos {
		utxo, err := unmarshalManagedUtxo(v)
		if err != nil {
			return nil, fmt.Errorf("unmarshal utxo %s: %w",
				k, err)
		}
		result[k] = utxo
	}

	return result, nil
}

// ListAssetGroups lists all known asset groups.
func (w *walletClient) ListAssetGroups(
	ctx context.Context) ([]tapsdk.AssetGroupRecord, error) {

	var resp jsonListGroupsResponse
	err := w.transport.doGet(
		ctx, "/v1/taproot-assets/assets/groups",
		macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make([]tapsdk.AssetGroupRecord, 0, len(resp.Groups))
	for k, v := range resp.Groups {
		group, err := unmarshalAssetGroupRecord(k, v)
		if err != nil {
			return nil, fmt.Errorf("unmarshal group %s: %w",
				k, err)
		}
		result = append(result, *group)
	}

	return result, nil
}

// BurnAsset burns asset units.
func (w *walletClient) BurnAsset(ctx context.Context,
	req *tapsdk.BurnAssetRequest) (
	*tapsdk.BurnAssetResponse, error) {

	body, err := burnAssetRequestBody(req)
	if err != nil {
		return nil, err
	}

	var resp jsonBurnAssetResponse
	err = w.transport.doPost(
		ctx, "/v1/taproot-assets/burn",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalBurnAssetResponse(&resp)
}

func burnAssetRequestBody(req *tapsdk.BurnAssetRequest) (map[string]any,
	error) {

	if req.AssetRef.IsZero() {
		return nil, fmt.Errorf("asset ref is required")
	}

	if err := req.AssetRef.Validate(); err != nil {
		return nil, err
	}

	body := map[string]any{
		"amount_to_burn":    fmt.Sprintf("%d", req.AmountToBurn),
		"confirmation_text": req.ConfirmationText,
	}

	switch {
	case req.AssetRef.IsGroupRef():
		groupKey, _ := req.AssetRef.GroupKey()
		body["asset_specifier"] = map[string]string{
			"group_key_str": hex.EncodeToString(groupKey[:]),
		}
	case req.AssetRef.IsAssetIDRef():
		assetID, _ := req.AssetRef.AssetID()
		body["asset_specifier"] = map[string]string{
			"asset_id_str": hex.EncodeToString(assetID[:]),
		}
	}

	if req.Note != "" {
		body["note"] = req.Note
	}

	return body, nil
}

// ListBurns lists asset burns with optional filtering.
func (w *walletClient) ListBurns(ctx context.Context,
	req *tapsdk.ListBurnsRequest) ([]*tapsdk.BurnRecord,
	error) {

	// grpc-gateway decodes query-string `bytes` fields as URL-safe
	// base64 (padding optional). Hex is rejected even though the
	// rest of tapd's REST surface uses UseHexForBytes for JSON
	// bodies.
	params := url.Values{}
	if req != nil {
		if req.AssetRef != nil {
			if err := req.AssetRef.Validate(); err != nil {
				return nil, err
			}

			if assetID, ok := req.AssetRef.AssetID(); ok {
				params.Set("asset_id",
					base64.URLEncoding.EncodeToString(
						assetID[:]))
			}

			if groupKey, ok := req.AssetRef.GroupKey(); ok {
				params.Set("tweaked_group_key",
					base64.URLEncoding.EncodeToString(
						groupKey[:]))
			}
		}
		if req.AnchorTxid != nil {
			params.Set("anchor_txid",
				base64.URLEncoding.EncodeToString(
					req.AnchorTxid[:],
				))
		}
	}

	path := "/v1/taproot-assets/burns"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp jsonListBurnsResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	burns := make([]*tapsdk.BurnRecord, 0, len(resp.Burns))
	for _, b := range resp.Burns {
		burn, err := unmarshalBurnRecord(b)
		if err != nil {
			return nil, err
		}
		burns = append(burns, burn)
	}

	return burns, nil
}

// FetchAssetMeta fetches the metadata for an asset.
func (w *walletClient) FetchAssetMeta(ctx context.Context,
	req *tapsdk.FetchAssetMetaRequest) (
	*tapsdk.AssetMeta, error) {

	var path string
	switch {
	case req.AssetRef != nil:
		if err := req.AssetRef.Validate(); err != nil {
			return nil, err
		}

		assetID, ok := req.AssetRef.AssetID()
		if !ok {
			return nil, fmt.Errorf("metadata lookup " +
				"requires an asset-ID ref; tapd does " +
				"not support group-key metadata lookup")
		}

		path = fmt.Sprintf(
			"/v1/taproot-assets/assets/meta/asset-id/%s",
			hex.EncodeToString(assetID[:]),
		)

	case req.MetaHash != nil:
		path = fmt.Sprintf(
			"/v1/taproot-assets/assets/meta/hash/%s",
			hex.EncodeToString(req.MetaHash[:]),
		)

	default:
		return nil, fmt.Errorf("either asset_ref or " +
			"meta_hash must be set")
	}

	var resp jsonFetchAssetMetaResponse
	err := w.transport.doGet(
		ctx, path, macaroon.AdminServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalFetchAssetMetaResponse(&resp)
}

// VerifyProof verifies a proof file.
func (w *walletClient) VerifyProof(ctx context.Context,
	rawProofFile []byte) (
	*tapsdk.VerifyProofResponse, error) {

	body := map[string]any{
		"raw_proof_file": hex.EncodeToString(
			rawProofFile,
		),
	}

	var resp jsonVerifyProofResponse
	err := w.transport.doPost(
		ctx, "/v1/taproot-assets/proofs/verify",
		macaroon.AdminServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalVerifyProofResponse(&resp)
}

// marshalAssetVersionJSON converts an AssetVersion to a proto JSON
// enum string.
func marshalAssetVersionJSON(v tapsdk.AssetVersion) string {
	switch v {
	case tapsdk.AssetVersionV1:
		return "ASSET_VERSION_V1"
	default:
		return "ASSET_VERSION_V0"
	}
}

// marshalAddressVersionJSON converts an AddressVersion to a proto
// JSON enum string.
func marshalAddressVersionJSON(v tapsdk.AddressVersion) string {
	switch v {
	case tapsdk.AddressVersionV1:
		return "ADDR_VERSION_V1"
	case tapsdk.AddressVersionV2:
		return "ADDR_VERSION_V2"
	default:
		return "ADDR_VERSION_V0"
	}
}

// marshalAddrEventStatusJSON converts an AddressEventStatus to a
// proto JSON enum string.
func marshalAddrEventStatusJSON(
	s tapsdk.AddressEventStatus) string {

	switch s {
	case tapsdk.AddressEventStatusTransactionDetected:
		return "ADDR_EVENT_STATUS_TRANSACTION_DETECTED"
	case tapsdk.AddressEventStatusTransactionConfirmed:
		return "ADDR_EVENT_STATUS_TRANSACTION_CONFIRMED"
	case tapsdk.AddressEventStatusProofReceived:
		return "ADDR_EVENT_STATUS_PROOF_RECEIVED"
	case tapsdk.AddressEventStatusCompleted:
		return "ADDR_EVENT_STATUS_COMPLETED"
	default:
		return "ADDR_EVENT_STATUS_UNKNOWN"
	}
}

// marshalScriptKeyJSON converts a ScriptKey to its JSON
// representation.
func marshalScriptKeyJSON(
	key *tapsdk.ScriptKey) *jsonScriptKey {

	if key == nil {
		return nil
	}

	result := &jsonScriptKey{
		PubKey:   hex.EncodeToString(key.PubKey[:]),
		TapTweak: hex.EncodeToString(key.TapTweak),
	}

	if key.KeyDesc != (tapsdk.KeyDescriptor{}) {
		result.KeyDesc = marshalKeyDescriptorJSON(&key.KeyDesc)
	}

	return result
}

// marshalKeyDescriptorJSON converts a KeyDescriptor to its JSON
// representation.
func marshalKeyDescriptorJSON(
	desc *tapsdk.KeyDescriptor) *jsonKeyDescriptor {

	if desc == nil {
		return nil
	}

	return &jsonKeyDescriptor{
		RawKeyBytes: hex.EncodeToString(desc.RawKeyBytes[:]),
		KeyLoc: &jsonKeyLocator{
			KeyFamily: int32(desc.KeyLocator.Family),
			KeyIndex:  int32(desc.KeyLocator.Index),
		},
	}
}
