package staticutil

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// ToWireOutpoints converts lnrpc.OutPoint protos into wire.OutPoint structs so
// they can be consumed by lower level transaction building code.
func ToWireOutpoints(outpoints []*lnrpc.OutPoint) ([]wire.OutPoint, error) {
	serverOutpoints := make([]wire.OutPoint, 0, len(outpoints))
	for _, o := range outpoints {
		outpointStr := fmt.Sprintf("%s:%d", o.TxidStr, o.OutputIndex)
		newOutpoint, err := wire.NewOutPointFromString(outpointStr)
		if err != nil {
			return nil, err
		}

		serverOutpoints = append(serverOutpoints, *newOutpoint)
	}

	return serverOutpoints, nil
}
