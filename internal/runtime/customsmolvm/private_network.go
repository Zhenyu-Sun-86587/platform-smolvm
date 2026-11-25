package customsmolvm

import (
	"fmt"
	"net/netip"

	rt "sandbox-platform/internal/runtime"
)

type PrivateNetwork struct {
	ID            string `yaml:"network_id"`
	RouteDomainID string `yaml:"route_domain_id"`
	CIDR          string `yaml:"cidr"`
}

func (r Route) validatePrivateNetworks() error {
	if len(r.PrivateNetworks) != 0 && r.WorkerSocket == "" {
		return fmt.Errorf("%w: private networks require a Worker socket", rt.ErrInvalid)
	}
	networks := map[string]bool{}
	domains := map[string]bool{}
	private := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	for _, n := range r.PrivateNetworks {
		prefix, err := netip.ParsePrefix(n.CIDR)
		inside := false
		for _, reserved := range private {
			inside = inside || (prefix.Bits() >= reserved.Bits() && reserved.Contains(prefix.Addr()))
		}
		if err != nil || !inside || prefix.Bits() > 30 || prefix.Masked().String() != n.CIDR ||
			!identifier.MatchString(n.ID) || !identifier.MatchString(n.RouteDomainID) || networks[n.ID] || domains[n.RouteDomainID] {
			return fmt.Errorf("%w: invalid or duplicate Worker private network", rt.ErrInvalid)
		}
		networks[n.ID], domains[n.RouteDomainID] = true, true
	}
	if r.DefaultNetworkID != "" && !networks[r.DefaultNetworkID] {
		return fmt.Errorf("%w: default private network is not configured", rt.ErrInvalid)
	}
	return nil
}

func (r Route) ResolvePrivateNetwork(selection *rt.LaunchNetwork) (*rt.LaunchNetwork, error) {
	if selection == nil && r.DefaultNetworkID == "" {
		return nil, nil
	}
	id, mode := r.DefaultNetworkID, rt.NativeNetwork
	if selection != nil {
		if selection.NetworkID != "" {
			id = selection.NetworkID
		}
		if selection.Mode != "" {
			mode = selection.Mode
		}
	}
	if mode != rt.NativeNetwork && mode != rt.FixedInternalNetwork {
		return nil, fmt.Errorf("%w: unknown private network mode", rt.ErrInvalid)
	}
	for _, network := range r.PrivateNetworks {
		if network.ID == id {
			return &rt.LaunchNetwork{NetworkID: id, Mode: mode, RouteDomainID: network.RouteDomainID, CIDR: network.CIDR}, nil
		}
	}
	return nil, fmt.Errorf("%w: private network is not registered for this runtime", rt.ErrInvalid)
}
