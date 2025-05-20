package assets

import (
	"context"

	clientrpc "github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
)

type AssetsClientServer struct {
	manager *AssetsSwapManager

	clientrpc.UnimplementedAssetsClientServer
}

func NewAssetsServer(manager *AssetsSwapManager) *AssetsClientServer {
	return &AssetsClientServer{
		manager: manager,
	}
}

func (a *AssetsClientServer) SwapOut(ctx context.Context,
	req *clientrpc.SwapOutRequest) (*clientrpc.SwapOutResponse, error) {

	swap, err := a.manager.NewSwapOut(
		ctx, req.Amt, req.Asset,
	)
	if err != nil {
		return nil, err
	}
	return &clientrpc.SwapOutResponse{
		SwapStatus: &clientrpc.AssetSwapStatus{
			SwapHash:   swap.SwapOut.SwapHash[:],
			SwapStatus: string(swap.SwapOut.State),
		},
	}, nil
}

func (a *AssetsClientServer) ListAssetSwaps(ctx context.Context,
	_ *clientrpc.ListAssetSwapsRequest) (*clientrpc.ListAssetSwapsResponse,
	error) {

	swaps, err := a.manager.ListSwapOuts(ctx)
	if err != nil {
		return nil, err
	}

	rpcSwaps := make([]*clientrpc.AssetSwapStatus, 0, len(swaps))
	for _, swap := range swaps {
		rpcSwaps = append(rpcSwaps, &clientrpc.AssetSwapStatus{
			SwapHash:   swap.SwapHash[:],
			SwapStatus: string(swap.State),
		})
	}

	return &clientrpc.ListAssetSwapsResponse{
		SwapStatus: rpcSwaps,
	}, nil
}

func (a *AssetsClientServer) ClientListAvailableAssets(ctx context.Context,
	req *clientrpc.ClientListAvailableAssetsRequest,
) (*clientrpc.ClientListAvailableAssetsResponse, error) {

	assets, err := a.manager.cfg.ServerClient.ListAvailableAssets(
		ctx, &swapserverrpc.ListAvailableAssetsRequest{},
	)
	if err != nil {
		return nil, err
	}

	availableAssets := make([]*clientrpc.Asset, 0, len(assets.Assets))

	for _, asset := range assets.Assets {
		clientAsset := &clientrpc.Asset{
			AssetId:     asset.AssetId,
			SatsPerUnit: asset.CurrentSatsPerAssetUnit,
			Name:        "Asset unknown in known universes",
		}
		universeRes, err := a.manager.cfg.AssetClient.QueryAssetRoots(
			ctx, &universerpc.AssetRootQuery{
				Id: &universerpc.ID{
					Id: &universerpc.ID_AssetId{
						AssetId: asset.AssetId,
					},
					ProofType: universerpc.ProofType_PROOF_TYPE_ISSUANCE,
				},
			},
		)
		if err != nil {
			return nil, err
		}

		if universeRes.IssuanceRoot != nil {
			clientAsset.Name = universeRes.IssuanceRoot.AssetName
		}

		availableAssets = append(availableAssets, clientAsset)
	}

	return &clientrpc.ClientListAvailableAssetsResponse{
		AvailableAssets: availableAssets,
	}, nil
}
func (a *AssetsClientServer) ClientGetAssetSwapOutQuote(ctx context.Context,
	req *clientrpc.ClientGetAssetSwapOutQuoteRequest,
) (*clientrpc.ClientGetAssetSwapOutQuoteResponse, error) {

	// Get the quote from the server.
	quoteRes, err := a.manager.cfg.ServerClient.QuoteAssetLoopOut(
		ctx, &swapserverrpc.QuoteAssetLoopOutRequest{
			Amount: req.Amt,
			Asset:  req.Asset,
		},
	)
	if err != nil {
		return nil, err
	}

	return &clientrpc.ClientGetAssetSwapOutQuoteResponse{
		SwapFee:     quoteRes.SwapFeeRate,
		PrepayAmt:   quoteRes.FixedPrepayAmt,
		SatsPerUnit: quoteRes.CurrentSatsPerAssetUnit,
	}, nil
}
