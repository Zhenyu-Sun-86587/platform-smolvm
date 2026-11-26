package nodeproto

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"

	"sandbox-platform/internal/runtime/workerclient"
)

const (
	BackupManifestPath   = Prefix + "/backup/manifest"
	BackupMemberPath     = Prefix + "/backup/member"
	BackupReleasePath    = Prefix + "/backup/release"
	BackupSizeHeader     = "X-Node-Backup-Bytes"
	BackupManifestHeader = "X-Node-Manifest-Digest"
	BackupDigestTrailer  = "X-Node-Content-Digest"
	BackupSelectionLimit = 2 << 20
)

func BackupSuffix(instance, bundle string) string {
	d := sha256.Sum256([]byte(instance + "\x00" + bundle))
	return hex.EncodeToString(d[:12])
}

func ValidMember(member string) bool {
	return len(member) < 1024 && path.Clean(member) == member && !strings.ContainsAny(member, "\\\x00") &&
		(strings.HasPrefix(member, "blocks/") || strings.HasPrefix(member, "statepoints/"))
}

type BackupSelection struct {
	ID          string                       `json:"backup_id"`
	Reader      string                       `json:"state_reader_id"`
	Captures    []workerclient.BackupCapture `json:"captures"`
	Generations []string                     `json:"generation_ids"`
}
type BackupMember struct {
	ID     string `json:"backup_id"`
	Member string `json:"member,omitempty"`
}
