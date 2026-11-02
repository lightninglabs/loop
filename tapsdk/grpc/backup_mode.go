package grpc

import (
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
)

func marshalBackupMode(mode tapsdk.BackupMode) assetwalletrpc.BackupMode {
	switch mode {
	case tapsdk.BackupModeCompact:
		return assetwalletrpc.BackupMode_COMPACT

	case tapsdk.BackupModeOptimistic:
		return assetwalletrpc.BackupMode_OPTIMISTIC

	default:
		return assetwalletrpc.BackupMode_RAW
	}
}
