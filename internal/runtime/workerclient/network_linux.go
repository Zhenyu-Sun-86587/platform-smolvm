package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"time"

	"golang.org/x/sys/unix"
)

type NetworkAttachment struct {
	Lease         NetworkLease `json:"lease"`
	RequestDigest string       `json:"request_digest"`
}

type NetworkLease struct {
	Execution
	LeaseID     string `json:"lease_id"`
	OperationID string `json:"operation_id"`
	Bundle      string `json:"bundle_digest"`
	Session     string `json:"broker_session_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
	Gate        string `json:"gate"`
	Attachment  struct {
		Schema  string     `json:"schema"`
		ID      string     `json:"attachment_id"`
		Address netip.Addr `json:"private_ipv4"`
	} `json:"attachment"`
}

func (c Client) LookupAttachment(ctx context.Context, execution Execution) (NetworkAttachment, error) {
	var a NetworkAttachment
	result, _, err := c.call(ctx, request{Method: "QueryExecutionNetworkAttachment", Expected: execution}, nil, nil)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(result, &a); err != nil {
		return a, err
	}
	lease := a.Lease
	if lease.Execution != execution || lease.Bundle != c.Bundle || lease.Revision < 1 ||
		lease.LeaseID == "" || lease.OperationID == "" || a.RequestDigest == "" ||
		lease.Attachment.Schema != "NetworkAttachment/1" || lease.Attachment.ID == "" ||
		!lease.Attachment.Address.Is4() || !lease.Attachment.Address.IsPrivate() {
		return NetworkAttachment{}, errors.New("Worker returned an attachment outside the execution scope")
	}
	return a, nil
}

func (c Client) DialAttachment(ctx context.Context, a NetworkAttachment, port uint16, transport string) (net.Conn, error) {
	lease := a.Lease
	if port == 0 || lease.Bundle != c.Bundle || lease.State != "attached" || lease.Gate != "open" ||
		(transport != "tcp" && transport != "udp") {
		return nil, errors.New("service requires a published private attachment")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	result, fd, err := c.call(ctx, request{
		Method: "DialNetworkAttachment", Expected: lease.Execution, OperationID: lease.OperationID, RequestDigest: a.RequestDigest,
		Payload: map[string]any{"attachment_id": lease.Attachment.ID, "lease_id": lease.LeaseID, "expected_revision": lease.Revision,
			"guest_port": port, "transport": transport},
	}, nil, &descriptor{ResourceID: lease.Attachment.ID, Role: "network_" + transport})
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	var observed NetworkLease
	if err := json.Unmarshal(result, &observed); err != nil {
		return nil, err
	}
	if observed != lease {
		return nil, errors.New("service dial changed its Worker attachment")
	}
	if err := waitConnected(ctx, int(fd.Fd())); err != nil {
		return nil, err
	}
	remote, err := unix.Getpeername(int(fd.Fd()))
	if err != nil {
		return nil, err
	}
	peer, ok := remote.(*unix.SockaddrInet4)
	if !ok || peer.Port != int(port) || netip.AddrFrom4(peer.Addr) != lease.Attachment.Address {
		return nil, errors.New("service socket differs from the attached private address")
	}
	return net.FileConn(fd)
}

func waitConnected(ctx context.Context, fd int) error {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
	for ctx.Err() == nil {
		n, err := unix.Poll(poll, 100)
		if err != nil && err != unix.EINTR {
			return err
		}
		if n == 0 || err == unix.EINTR {
			continue
		}
		status, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if status != 0 {
			return unix.Errno(status)
		}
		return nil
	}
	return ctx.Err()
}
