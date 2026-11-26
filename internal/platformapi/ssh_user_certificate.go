package platformapi

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
)

const managedSSHCertificateRequestLimit = 8 << 10

func managedSSHPublicKey(text string) (ssh.PublicKey, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "\r\n\x00") {
		return nil, registry.ErrInvalid
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(options) != 0 || len(rest) != 0 {
		return nil, registry.ErrInvalid
	}
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return key, nil
	case ssh.KeyAlgoRSA:
		if pub, ok := key.(ssh.CryptoPublicKey).CryptoPublicKey().(*rsa.PublicKey); ok && pub.N.BitLen() >= 2048 && pub.N.BitLen() <= 8192 {
			return key, nil
		}
	}
	return nil, registry.ErrInvalid
}

func (s *Server) issueManagedSSHCertificate(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	mid := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, mid); err != nil {
		return err
	}
	var request struct {
		PublicKey string `json:"public_key"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, managedSSHCertificateRequestLimit)
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return registry.ErrInvalid
		}
		return err
	}
	key, err := managedSSHPublicKey(request.PublicKey)
	if err != nil {
		return err
	}
	binding, err := s.ManagedSystemSSHBinding(r.Context(), mid)
	if err != nil {
		return err
	}
	source, err := s.managedSSHRouterSource(r.Context(), binding)
	if err != nil {
		return err
	}
	if err := s.CheckAccessPrincipal(r.Context(), p, mid); err != nil {
		return err
	}
	certificate, expires, err := s.SignManagedSSHCertificate(mid, key, source)
	if err != nil {
		return err
	}
	return write(w, http.StatusOK, struct {
		Certificate string    `json:"certificate"`
		ExpiresAt   time.Time `json:"expires_at"`
	}{Certificate: certificate, ExpiresAt: expires})
}

func (s *Server) managedSSHRouterSource(ctx context.Context, binding registry.Binding) (netip.Prefix, error) {
	unverified := &lifecycle.Failure{Code: "SSH_ROUTER_UNVERIFIED", Message: "the managed SSH router could not be verified for this machine"}
	machine, err := s.DB.GetMachine(ctx, binding.MachineID)
	if err != nil {
		return netip.Prefix{}, err
	}
	b := machine.CurrentBinding
	if machine.DeletedAt != nil || b == nil || b.ID != binding.ID || b.Generation != binding.Generation ||
		b.ExecutionRef() != binding.ExecutionRef() || b.PowerState != binding.PowerState {
		return netip.Prefix{}, registry.ErrGeneration
	}
	var spec struct {
		Network *rt.LaunchNetwork `json:"network"`
	}
	if err := json.Unmarshal(machine.Spec, &spec); err != nil || spec.Network == nil {
		return netip.Prefix{}, unverified
	}
	adapter, err := s.ForBinding(binding)
	if err != nil {
		return netip.Prefix{}, err
	}
	socket := privateServiceWorker(adapter)
	if socket == "" {
		return netip.Prefix{}, unverified
	}
	var networkID string
	var address netip.Addr
	switch binding.PowerState {
	case registry.PowerRunning, registry.PowerStandby:
		_, attachment, err := workerServiceAttachment(ctx, socket, binding)
		if err != nil || attachment.Lease.Attachment.Mode != spec.Network.Mode {
			return netip.Prefix{}, unverified
		}
		networkID, address = attachment.NetworkID, attachment.Lease.Attachment.Address
	case registry.PowerHibernated, registry.PowerStopped:
		saved, err := s.packetWakeEndpoint(ctx, binding.MachineID)
		if err != nil || saved.client.Socket != socket || saved.client.Bundle != binding.RuntimeBundleID ||
			saved.generation != binding.Generation || saved.execution.InstanceID != binding.RuntimeInstanceObjectID ||
			saved.execution.Epoch != binding.ExecutionEpoch || saved.execution.IncarnationID != "" {
			return netip.Prefix{}, unverified
		}
		networkID = saved.endpoint.NetworkID
		address, _ = netip.ParseAddr(saved.endpoint.PrivateIPv4)
	default:
		return netip.Prefix{}, unverified
	}
	source := sshRouterPrefix(s.Config.Runtime.Custom, binding, socket, *spec.Network, networkID, address)
	if !source.IsValid() {
		return netip.Prefix{}, unverified
	}
	return source, nil
}

// The Worker reserves the first usable address for its router. Requiring its
// selected network to agree with the persisted launch and operator configuration
// prevents deriving the router address from a changed CIDR or a Guest address.
func sshRouterPrefix(routes []customsmolvm.Route, binding registry.Binding, socket string, launch rt.LaunchNetwork, networkID string, privateIP netip.Addr) netip.Prefix {
	if networkID == "" || networkID != launch.NetworkID || launch.RouteDomainID == "" || !privateIP.Is4() ||
		(launch.Mode != rt.NativeNetwork && launch.Mode != rt.FixedInternalNetwork) {
		return netip.Prefix{}
	}
	want := rt.BundleRoute{InstanceID: binding.RuntimeInstanceID, BundleID: binding.RuntimeBundleID}
	var route *customsmolvm.Route
	for i := range routes {
		if routes[i].Key() == want {
			if route != nil {
				return netip.Prefix{}
			}
			route = &routes[i]
		}
	}
	if route == nil || route.WorkerSocket != socket {
		return netip.Prefix{}
	}
	var cidr string
	for _, network := range route.PrivateNetworks {
		if network.ID == networkID {
			if cidr != "" || network.CIDR == "" || network.RouteDomainID != launch.RouteDomainID || network.CIDR != launch.CIDR {
				return netip.Prefix{}
			}
			cidr = network.CIDR
		}
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || prefix.Masked().String() != cidr || prefix.Bits() > 30 || !prefix.Contains(privateIP) ||
		!prefix.Contains(privateIP.Next()) {
		return netip.Prefix{}
	}
	router := prefix.Addr().Next()
	if !router.IsPrivate() || privateIP.Compare(router) <= 0 {
		return netip.Prefix{}
	}
	for _, private := range []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	} {
		if prefix.Bits() >= private.Bits() && private.Contains(prefix.Addr()) {
			return netip.PrefixFrom(router, 32)
		}
	}
	return netip.Prefix{}
}
