package tapsdk

// BackupMode specifies the backup format to use when exporting an asset
// wallet backup.
type BackupMode uint8

const (
	// BackupModeRaw exports a full backup with complete proof data.
	BackupModeRaw BackupMode = iota

	// BackupModeCompact strips blockchain-derivable proof fields to reduce
	// backup size.
	BackupModeCompact

	// BackupModeOptimistic omits proof data and relies on universe fetches
	// during restore.
	BackupModeOptimistic
)

// ExportBackupRequest specifies the desired backup mode.
type ExportBackupRequest struct {
	Mode BackupMode
}

// ExportBackupResponse returns the serialized backup blob.
type ExportBackupResponse struct {
	Backup []byte
}

// ImportBackupRequest provides a previously exported backup blob.
type ImportBackupRequest struct {
	Backup []byte
}

// ImportBackupResponse reports how many assets were imported.
type ImportBackupResponse struct {
	NumImported uint32
}
