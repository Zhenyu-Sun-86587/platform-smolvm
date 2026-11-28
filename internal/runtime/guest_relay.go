package runtime

import (
	"context"
	"net"
)

// GuestRelayScope binds an inbound Guest connection to one known execution.
// Name is the registered Runtime object name used to verify the execution.
type GuestRelayScope struct {
	BindingID string       `json:"binding_id"`
	Name      string       `json:"name"`
	Execution ExecutionRef `json:"execution"`
}

// GuestRelayBridge accepts a Worker-verified Guest connection over an
// authenticated transport. The authenticated listener serves only Guest-grant
// calls and carries no reusable attestation token.
type GuestRelayBridge interface {
	GuestRelaySupported() bool
	AcceptGuestRelay(context.Context, GuestRelayScope) (net.Conn, error)
	AuthorizeGuestRelay(context.Context) error
}
