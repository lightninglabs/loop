package rest

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
)

// universeClient implements tapsdk.UniverseClient over REST.
type universeClient struct {
	transport *transport
}

func newUniverseClient(tp *transport) *universeClient {
	return &universeClient{transport: tp}
}

// jsonUniverseProof is the JSON body for InsertProof.
type jsonUniverseProof struct {
	Key       *jsonUniverseKey  `json:"key,omitempty"`
	AssetLeaf *jsonAssetLeafReq `json:"asset_leaf"`
}

// jsonUniverseKey is the JSON shape of universerpc.UniverseKey.
type jsonUniverseKey struct {
	ID      *jsonUniverseID `json:"id"`
	LeafKey *jsonAssetKey   `json:"leaf_key"`
}

// jsonUniverseID is the JSON shape of universerpc.ID.
type jsonUniverseID struct {
	AssetID   string `json:"asset_id,omitempty"`
	GroupKey  string `json:"group_key,omitempty"`
	ProofType string `json:"proof_type"`
}

// jsonAssetLeafReq is the JSON shape of universerpc.AssetLeaf.
type jsonAssetLeafReq struct {
	Proof string `json:"proof"`
}

const (
	// proofTypeIssuance is the proto enum string for issuance
	// proofs.
	proofTypeIssuance = "PROOF_TYPE_ISSUANCE"

	// proofTypeTransfer is the proto enum string for transfer
	// proofs.
	proofTypeTransfer = "PROOF_TYPE_TRANSFER"

	// proofTypeUnspecified is the proto enum string for
	// unspecified proof types.
	proofTypeUnspecified = "PROOF_TYPE_UNSPECIFIED"
)

// marshalProofType converts an tapsdk.ProofType to the proto enum
// string.
func marshalProofType(pt tapsdk.ProofType) string {
	switch pt {
	case tapsdk.ProofTypeIssuance:
		return proofTypeIssuance
	case tapsdk.ProofTypeTransfer:
		return proofTypeTransfer
	default:
		return proofTypeUnspecified
	}
}

// marshalSortDirection converts an tapsdk.SortDirection to the proto
// enum string.
func marshalSortDirection(sd tapsdk.SortDirection) string {
	switch sd {
	case tapsdk.SortAscending:
		return "SORT_DIRECTION_ASC"
	default:
		return "SORT_DIRECTION_DESC"
	}
}

// marshalAssetQuerySort converts an tapsdk.AssetQuerySort to the
// proto enum string.
func marshalAssetQuerySort(s tapsdk.AssetQuerySort) string {
	switch s {
	case tapsdk.SortByAssetName:
		return "SORT_BY_ASSET_NAME"
	case tapsdk.SortByIssuanceID:
		return "SORT_BY_ASSET_ID"
	case tapsdk.SortByAssetType:
		return "SORT_BY_ASSET_TYPE"
	case tapsdk.SortByTotalSyncs:
		return "SORT_BY_TOTAL_SYNCS"
	case tapsdk.SortByTotalProofs:
		return "SORT_BY_TOTAL_PROOFS"
	case tapsdk.SortByGenesisHeight:
		return "SORT_BY_GENESIS_HEIGHT"
	case tapsdk.SortByTotalSupply:
		return "SORT_BY_TOTAL_SUPPLY"
	default:
		return "SORT_BY_NONE"
	}
}

// marshalAssetTypeFilter converts an tapsdk.AssetTypeFilter to the
// proto enum string.
func marshalAssetTypeFilter(f tapsdk.AssetTypeFilter) string {
	switch f {
	case tapsdk.FilterAssetNormal:
		return "FILTER_ASSET_NORMAL"
	case tapsdk.FilterAssetCollectible:
		return "FILTER_ASSET_COLLECTIBLE"
	default:
		return "FILTER_ASSET_NONE"
	}
}

// marshalSyncMode converts an tapsdk.UniverseSyncMode to the proto
// enum string.
func marshalSyncMode(m tapsdk.UniverseSyncMode) string {
	switch m {
	case tapsdk.SyncFull:
		return "SYNC_FULL"
	default:
		return "SYNC_ISSUANCE_ONLY"
	}
}

// InsertProof inserts a proof into the local universe.
func (u *universeClient) InsertProof(ctx context.Context,
	rawProof []byte,
	decoded *tapsdk.DecodedProof) error {

	proofType := proofTypeTransfer
	if decoded.IsIssuance {
		proofType = proofTypeIssuance
	}

	assetPathKind, assetPathValue, err := universeAssetPath(
		decoded.AssetRef,
	)
	if err != nil {
		return err
	}

	// Build the REST URL path. InsertProof uses a POST with
	// path parameters for the universe key.
	// The gRPC-gateway REST path uses the human-readable txid
	// (reversed byte order) and output index from the outpoint.
	outpointStr := decoded.Outpoint.String()
	parts := splitOutpoint(outpointStr)
	hashStr := parts[0]
	index := decoded.Outpoint.Index
	scriptKeyStr := hex.EncodeToString(decoded.ScriptKey[:])

	path := fmt.Sprintf(
		"/v1/taproot-assets/universe/proofs/"+
			"%s/%s/%s/%d/%s",
		assetPathKind, assetPathValue, hashStr, index, scriptKeyStr,
	)

	body := &jsonUniverseProof{
		Key: &jsonUniverseKey{
			ID: &jsonUniverseID{
				ProofType: proofType,
			},
		},
		AssetLeaf: &jsonAssetLeafReq{
			Proof: hex.EncodeToString(rawProof),
		},
	}

	var resp jsonInsertProofResponse
	err = u.transport.doPost(
		ctx, path, macaroon.UniverseServiceMac, body, &resp,
	)
	if err != nil {
		return err
	}

	return nil
}

// splitOutpoint splits "txid:index" into its parts, returning
// [txid, index]. If no colon found, returns the original string
// and "0".
func splitOutpoint(s string) [2]string {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return [2]string{s[:i], s[i+1:]}
		}
	}

	return [2]string{s, "0"}
}

func universeAssetPath(assetRef tapsdk.AssetRef) (string, string, error) {
	if err := assetRef.Validate(); err != nil {
		return "", "", err
	}

	if assetID, ok := assetRef.AssetID(); ok {
		return "asset-id", hex.EncodeToString(assetID[:]), nil
	}

	if groupKey, ok := assetRef.GroupKey(); ok {
		return "group-key", hex.EncodeToString(groupKey[:]), nil
	}

	return "", "", fmt.Errorf("asset ref is required")
}

func universeJSONID(id tapsdk.UniverseID) (*jsonUniverseID, error) {
	jsonID := &jsonUniverseID{ProofType: marshalProofType(id.ProofType)}

	if err := id.AssetRef.Validate(); err != nil {
		return nil, err
	}

	if assetID, ok := id.AssetRef.AssetID(); ok {
		jsonID.AssetID = hex.EncodeToString(assetID[:])
	}

	if groupKey, ok := id.AssetRef.GroupKey(); ok {
		jsonID.GroupKey = hex.EncodeToString(groupKey[:])
	}

	return jsonID, nil
}

// AssetRoots returns the known universe roots for all assets.
func (u *universeClient) AssetRoots(ctx context.Context,
	req *tapsdk.AssetRootRequest) (
	map[string]*tapsdk.UniverseRoot, error) {

	params := ""
	if req != nil {
		p := make(map[string]string)
		if req.WithAmountsByID {
			p["with_amounts_by_id"] = queryTrue
		}
		if req.Offset != 0 {
			p["offset"] = fmt.Sprintf("%d", req.Offset)
		}
		if req.Limit != 0 {
			p["limit"] = fmt.Sprintf("%d", req.Limit)
		}
		if req.Direction != 0 {
			p["direction"] = marshalSortDirection(
				req.Direction,
			)
		}

		if len(p) > 0 {
			var b strings.Builder
			first := true
			for k, v := range p {
				if first {
					b.WriteString("?")
					first = false
				} else {
					b.WriteString("&")
				}
				b.WriteString(k)
				b.WriteString("=")
				b.WriteString(v)
			}
			params = b.String()
		}
	}

	path := "/v1/taproot-assets/universe/roots" + params

	var resp jsonAssetRootsResponse
	err := u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*tapsdk.UniverseRoot)
	for k, v := range resp.UniverseRoots {
		root, err := unmarshalUniverseRoot(v)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal root %s: %w", k, err,
			)
		}
		result[k] = root
	}

	return result, nil
}

// QueryAssetRoots queries the issuance and transfer roots for a
// specific asset.
func (u *universeClient) QueryAssetRoots(ctx context.Context,
	id *tapsdk.UniverseID) (*tapsdk.QueryRootResponse,
	error) {

	if id == nil {
		return nil, fmt.Errorf("nil universe ID")
	}

	pathKind, pathValue, err := universeAssetPath(id.AssetRef)
	if err != nil {
		return nil, err
	}

	var path string
	proofTypeParam := "?id.proof_type=" + marshalProofType(
		id.ProofType,
	)
	path = fmt.Sprintf(
		"/v1/taproot-assets/universe/roots/%s/%s%s",
		pathKind, pathValue, proofTypeParam,
	)

	var resp jsonQueryRootResponse
	err = u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	issuanceRoot, err := unmarshalUniverseRoot(resp.IssuanceRoot)
	if err != nil {
		return nil, fmt.Errorf("unmarshal issuance root: %w", err)
	}

	transferRoot, err := unmarshalUniverseRoot(resp.TransferRoot)
	if err != nil {
		return nil, fmt.Errorf("unmarshal transfer root: %w", err)
	}

	return &tapsdk.QueryRootResponse{
		IssuanceRoot: issuanceRoot,
		TransferRoot: transferRoot,
	}, nil
}

// DeleteAssetRoot deletes a universe root and all associated data.
func (u *universeClient) DeleteAssetRoot(ctx context.Context,
	id *tapsdk.UniverseID) error {

	if id == nil {
		return fmt.Errorf("nil universe ID")
	}

	params := "?id.proof_type=" + marshalProofType(id.ProofType)
	jsonID, err := universeJSONID(*id)
	if err != nil {
		return err
	}
	if jsonID.AssetID != "" {
		params += "&id.asset_id_str=" + jsonID.AssetID
	}
	if jsonID.GroupKey != "" {
		params += "&id.group_key_str=" + jsonID.GroupKey
	}

	path := "/v1/taproot-assets/universe/delete" + params

	var resp jsonDeleteRootResponse
	err = u.transport.do(
		ctx, "DELETE", path, macaroon.UniverseServiceMac, nil,
		&resp,
	)
	if err != nil {
		return err
	}

	return nil
}

// AssetLeafKeys returns the set of leaf keys for a universe.
func (u *universeClient) AssetLeafKeys(ctx context.Context,
	req *tapsdk.AssetLeafKeysRequest) (
	[]tapsdk.AssetLeafKey, error) {

	if req == nil {
		return nil, fmt.Errorf("nil request")
	}

	pathKind, pathValue, err := universeAssetPath(req.ID.AssetRef)
	if err != nil {
		return nil, err
	}

	basePath := fmt.Sprintf(
		"/v1/taproot-assets/universe/keys/%s/%s",
		pathKind, pathValue,
	)

	params := "?id.proof_type=" + marshalProofType(
		req.ID.ProofType,
	)

	if req.Offset != 0 {
		params += fmt.Sprintf("&offset=%d", req.Offset)
	}
	if req.Limit != 0 {
		params += fmt.Sprintf("&limit=%d", req.Limit)
	}
	if req.Direction != 0 {
		params += "&direction=" + marshalSortDirection(
			req.Direction,
		)
	}

	path := basePath + params

	var resp jsonAssetLeafKeysResponse
	err = u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make([]tapsdk.AssetLeafKey, 0, len(resp.AssetKeys))
	for _, k := range resp.AssetKeys {
		leafKey, err := unmarshalAssetLeafKey(k)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal asset key: %w", err,
			)
		}
		result = append(result, *leafKey)
	}

	return result, nil
}

// AssetLeaves returns the set of asset leaves for a universe.
func (u *universeClient) AssetLeaves(ctx context.Context,
	id *tapsdk.UniverseID) ([]tapsdk.AssetLeaf, error) {

	if id == nil {
		return nil, fmt.Errorf("nil universe ID")
	}

	pathKind, pathValue, err := universeAssetPath(id.AssetRef)
	if err != nil {
		return nil, err
	}

	var path string
	proofTypeParam := "?id.proof_type=" + marshalProofType(
		id.ProofType,
	)
	path = fmt.Sprintf(
		"/v1/taproot-assets/universe/leaves/%s/%s%s",
		pathKind, pathValue, proofTypeParam,
	)

	var resp jsonAssetLeavesResponse
	err = u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make([]tapsdk.AssetLeaf, 0, len(resp.Leaves))
	for _, l := range resp.Leaves {
		leaf, err := unmarshalAssetLeaf(l)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal asset leaf: %w", err,
			)
		}
		result = append(result, *leaf)
	}

	return result, nil
}

// QueryProof queries a specific proof from the universe.
func (u *universeClient) QueryProof(ctx context.Context,
	key *tapsdk.UniverseKey) (*tapsdk.AssetProofResponse,
	error) {

	if key == nil {
		return nil, fmt.Errorf("nil universe key")
	}

	pathKind, pathValue, err := universeAssetPath(key.ID.AssetRef)
	if err != nil {
		return nil, err
	}

	basePath := fmt.Sprintf(
		"/v1/taproot-assets/universe/proofs/%s/%s",
		pathKind, pathValue,
	)

	outpointParts := splitOutpoint(key.LeafKey.Outpoint.String())
	hashStr := outpointParts[0]
	index := key.LeafKey.Outpoint.Index
	scriptKeyStr := hex.EncodeToString(key.LeafKey.ScriptKey[:])

	path := fmt.Sprintf(
		"%s/%s/%d/%s?id.proof_type=%s",
		basePath, hashStr, index, scriptKeyStr,
		marshalProofType(key.ID.ProofType),
	)

	var resp jsonQueryProofResponse
	err = u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalAssetProofResponse(&resp)
}

// UniverseStats returns aggregate statistics for the universe.
func (u *universeClient) UniverseStats(
	ctx context.Context) (*tapsdk.UniverseStats, error) {

	var resp jsonUniverseStatsResponse
	err := u.transport.doGet(
		ctx, "/v1/taproot-assets/universe/stats",
		macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	return unmarshalUniverseStats(&resp)
}

// QueryAssetStats returns per-asset statistics.
func (u *universeClient) QueryAssetStats(ctx context.Context,
	req *tapsdk.AssetStatsQuery) (
	[]tapsdk.AssetStatsSnapshot, error) {

	params := ""
	if req != nil {
		p := make([]string, 0)

		if req.AssetNameFilter != "" {
			p = append(
				p, "asset_name_filter="+
					req.AssetNameFilter,
			)
		}

		if req.AssetRefFilter != nil {
			if err := req.AssetRefFilter.Validate(); err != nil {
				return nil, err
			}

			if assetID, ok := req.AssetRefFilter.AssetID(); ok {
				assetIDStr := hex.EncodeToString(assetID[:])
				p = append(p, "asset_id_filter="+assetIDStr)
			}

			// NOTE: tapd's QueryAssetStats does not support
			// group-key filtering. A group-key AssetRef is
			// silently ignored here; the caller should
			// filter client-side or use an asset-ID ref.
		}

		if req.AssetTypeFilter != 0 {
			p = append(
				p, "asset_type_filter="+
					marshalAssetTypeFilter(
						req.AssetTypeFilter,
					),
			)
		}

		if req.SortBy != 0 {
			p = append(
				p, "sort_by="+marshalAssetQuerySort(
					req.SortBy,
				),
			)
		}

		if req.Offset != 0 {
			p = append(
				p, fmt.Sprintf("offset=%d", req.Offset),
			)
		}

		if req.Limit != 0 {
			p = append(
				p, fmt.Sprintf("limit=%d", req.Limit),
			)
		}

		if req.Direction != 0 {
			p = append(
				p, "direction="+marshalSortDirection(
					req.Direction,
				),
			)
		}

		if len(p) > 0 {
			params = "?" + p[0]
			for i := 1; i < len(p); i++ {
				params += "&" + p[i]
			}
		}
	}

	path := "/v1/taproot-assets/universe/stats/assets" + params

	var resp jsonAssetStatsResponse
	err := u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(
		[]tapsdk.AssetStatsSnapshot, 0, len(resp.AssetStats),
	)
	for _, s := range resp.AssetStats {
		snapshot, err := unmarshalAssetStatsSnapshot(s)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal asset stats snapshot: %w", err,
			)
		}
		result = append(result, *snapshot)
	}

	return result, nil
}

// QueryEvents returns daily sync and proof event counts.
func (u *universeClient) QueryEvents(ctx context.Context,
	req *tapsdk.QueryEventsRequest) (
	[]tapsdk.GroupedUniverseEvents, error) {

	params := ""
	if req != nil {
		params = fmt.Sprintf(
			"?start_timestamp=%d&end_timestamp=%d",
			req.StartTimestamp, req.EndTimestamp,
		)
	}

	path := "/v1/taproot-assets/universe/stats/events" + params

	var resp jsonQueryEventsResponse
	err := u.transport.doGet(
		ctx, path, macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(
		[]tapsdk.GroupedUniverseEvents, 0, len(resp.Events),
	)
	for _, e := range resp.Events {
		event, err := unmarshalGroupedUniverseEvents(e)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal grouped events: %w", err,
			)
		}
		result = append(result, *event)
	}

	return result, nil
}

// ListFederationServers lists the universe federation peers.
func (u *universeClient) ListFederationServers(
	ctx context.Context) ([]tapsdk.FederationServer, error) {

	var resp jsonListFederationServersResponse
	err := u.transport.doGet(
		ctx, "/v1/taproot-assets/universe/federation",
		macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(
		[]tapsdk.FederationServer, 0, len(resp.Servers),
	)
	for _, s := range resp.Servers {
		server, err := unmarshalFederationServer(s)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal federation server: %w", err,
			)
		}
		result = append(result, *server)
	}

	return result, nil
}

// AddFederationServer adds servers to the federation.
func (u *universeClient) AddFederationServer(ctx context.Context,
	servers []tapsdk.FederationServer) error {

	jsonServers := make(
		[]*jsonUniverseFederationServer, 0, len(servers),
	)
	for _, s := range servers {
		jsonServers = append(jsonServers,
			&jsonUniverseFederationServer{
				Host: s.Host,
				ID:   s.ID,
			},
		)
	}

	body := &jsonAddFederationServerRequest{
		Servers: jsonServers,
	}

	var resp jsonAddFederationServerResponse
	err := u.transport.doPost(
		ctx, "/v1/taproot-assets/universe/federation",
		macaroon.UniverseServiceMac, body, &resp,
	)
	if err != nil {
		return err
	}

	return nil
}

// DeleteFederationServer removes servers from the federation.
func (u *universeClient) DeleteFederationServer(ctx context.Context,
	servers []tapsdk.FederationServer) error {

	jsonServers := make(
		[]*jsonUniverseFederationServer, 0, len(servers),
	)
	for _, s := range servers {
		jsonServers = append(jsonServers,
			&jsonUniverseFederationServer{
				Host: s.Host,
				ID:   s.ID,
			},
		)
	}

	body := &jsonDeleteFederationServerRequest{
		Servers: jsonServers,
	}

	var resp jsonDeleteFederationServerResponse
	err := u.transport.do(
		ctx, "DELETE",
		"/v1/taproot-assets/universe/federation",
		macaroon.UniverseServiceMac, body, &resp,
	)
	if err != nil {
		return err
	}

	return nil
}

// SetFederationSyncConfig sets the federation sync configuration.
func (u *universeClient) SetFederationSyncConfig(
	ctx context.Context,
	global []tapsdk.GlobalFederationSyncConfig,
	asset []tapsdk.AssetFederationSyncConfig) error {

	jsonGlobal := make(
		[]*jsonGlobalFederationSyncConfig, 0, len(global),
	)
	for _, g := range global {
		jsonGlobal = append(jsonGlobal,
			&jsonGlobalFederationSyncConfig{
				ProofType: marshalProofType(
					g.ProofType,
				),
				AllowSyncInsert: g.AllowSyncInsert,
				AllowSyncExport: g.AllowSyncExport,
			},
		)
	}

	jsonAsset := make(
		[]*jsonAssetFederationSyncConfig, 0, len(asset),
	)
	for _, a := range asset {
		id, err := universeJSONID(a.ID)
		if err != nil {
			return err
		}

		jsonAsset = append(jsonAsset,
			&jsonAssetFederationSyncConfig{
				ID:              id,
				AllowSyncInsert: a.AllowSyncInsert,
				AllowSyncExport: a.AllowSyncExport,
			},
		)
	}

	body := &jsonSetFederationSyncConfigRequest{
		GlobalSyncConfigs: jsonGlobal,
		AssetSyncConfigs:  jsonAsset,
	}

	var resp jsonSetFederationSyncConfigResponse
	err := u.transport.doPost(
		ctx, "/v1/taproot-assets/universe/sync/config",
		macaroon.UniverseServiceMac, body, &resp,
	)
	if err != nil {
		return err
	}

	return nil
}

// QueryFederationSyncConfig queries the federation sync config.
func (u *universeClient) QueryFederationSyncConfig(
	ctx context.Context,
	ids []tapsdk.UniverseID) (
	*tapsdk.FederationSyncConfig, error) {

	var resp jsonQueryFederationSyncConfigResponse
	err := u.transport.doGet(
		ctx, "/v1/taproot-assets/universe/sync/config",
		macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	globalConfigs := make(
		[]tapsdk.GlobalFederationSyncConfig, 0,
		len(resp.GlobalSyncConfigs),
	)
	for _, g := range resp.GlobalSyncConfigs {
		cfg, err := unmarshalGlobalFederationSyncConfig(g)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal global sync config: %w", err,
			)
		}
		globalConfigs = append(globalConfigs, *cfg)
	}

	assetConfigs := make(
		[]tapsdk.AssetFederationSyncConfig, 0,
		len(resp.AssetSyncConfigs),
	)
	for _, a := range resp.AssetSyncConfigs {
		cfg, err := unmarshalAssetFederationSyncConfig(a)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal asset sync config: %w", err,
			)
		}
		assetConfigs = append(assetConfigs, *cfg)
	}

	return &tapsdk.FederationSyncConfig{
		GlobalSyncConfigs: globalConfigs,
		AssetSyncConfigs:  assetConfigs,
	}, nil
}

// Info returns basic universe server information.
func (u *universeClient) Info(
	ctx context.Context) (*tapsdk.UniverseInfo, error) {

	var resp jsonInfoResponse
	err := u.transport.doGet(
		ctx, "/v1/taproot-assets/universe/info",
		macaroon.UniverseServiceMac, &resp,
	)
	if err != nil {
		return nil, err
	}

	runtimeID, err := parseInt64(resp.RuntimeID)
	if err != nil {
		return nil, fmt.Errorf("invalid runtime_id: %w", err)
	}

	return &tapsdk.UniverseInfo{
		RuntimeID: runtimeID,
	}, nil
}

// SyncUniverse synchronizes with a remote universe server.
func (u *universeClient) SyncUniverse(ctx context.Context,
	req *tapsdk.SyncRequest) (
	[]tapsdk.SyncedUniverse, error) {

	if req == nil {
		return nil, fmt.Errorf("nil sync request")
	}

	jsonTargets := make([]*jsonSyncTarget, 0, len(req.SyncTargets))
	for _, t := range req.SyncTargets {
		id, err := universeJSONID(t.ID)
		if err != nil {
			return nil, err
		}

		jsonTargets = append(jsonTargets, &jsonSyncTarget{
			ID: id,
		})
	}

	body := &jsonSyncUniverseRequest{
		UniverseHost: req.UniverseHost,
		SyncMode:     marshalSyncMode(req.SyncMode),
		SyncTargets:  jsonTargets,
	}

	var resp jsonSyncUniverseResponse
	err := u.transport.doPost(
		ctx, "/v1/taproot-assets/universe/sync",
		macaroon.UniverseServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	result := make(
		[]tapsdk.SyncedUniverse, 0,
		len(resp.SyncedUniverses),
	)
	for _, s := range resp.SyncedUniverses {
		synced, err := unmarshalSyncedUniverse(s)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal synced universe: %w", err,
			)
		}
		result = append(result, *synced)
	}

	return result, nil
}
