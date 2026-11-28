package nodeproto

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ManagedInstanceID follows the measured Serve's managed::machine_id contract.
func ManagedInstanceID(localInstanceID, name string) string {
	digest := sha256.Sum256([]byte(name))
	return "i-" + localInstanceID + "-" + hex.EncodeToString(digest[:])
}

// OwnsManagedInstance follows the Serve's require_own namespace check. Bundle
// identity alone cannot distinguish two private Serve processes on one Worker.
func OwnsManagedInstance(localInstanceID, instanceID string) bool {
	tail, ok := strings.CutPrefix(instanceID, "i-"+localInstanceID+"-")
	if !ok || !ValidID(localInstanceID) || !ValidID(instanceID) || len(tail) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(tail)
	return err == nil
}
