package nodeproto

import "regexp"

var journalIDPattern = regexp.MustCompile(`^journal-[0-9a-f]{64}$`)

func ValidJournalID(id string) bool {
	return journalIDPattern.MatchString(id)
}

// NodeIdentity binds a command to a persistent journal and one live process.
type NodeIdentity struct {
	JournalID   string `json:"journal_id"`
	NodeSession string `json:"node_session"`
}

func (id NodeIdentity) Valid() bool {
	return ValidJournalID(id.JournalID) && ValidID(id.NodeSession)
}

func (h Handshake) Identity() NodeIdentity {
	return NodeIdentity{JournalID: h.JournalID, NodeSession: h.NodeSession}
}
