package server

import (
	"github.com/pkg/errors"
	"net/netip"
	"strings"
)

type addrMatcher struct {
	addrs    []netip.Addr
	prefixes []netip.Prefix
}

func newAddrMatcher(filters []string) (*addrMatcher, error) {
	addrs := make([]netip.Addr, 0)
	prefixes := make([]netip.Prefix, 0)

	if filters != nil {
		for _, filter := range filters {
			if strings.Contains(filter, "/") {
				prefix, err := netip.ParsePrefix(filter)
				if err != nil {
					return nil, err
				}
				prefixes = append(prefixes, prefix)
			} else {
				addr, err := netip.ParseAddr(filter)
				if err != nil {
					return nil, err
				}
				addrs = append(addrs, addr)
			}
		}
	}

	return &addrMatcher{
		addrs:    addrs,
		prefixes: prefixes,
	}, nil
}

func (a *addrMatcher) Match(addr netip.Addr) bool {
	for _, a := range a.addrs {

		// Before comparison, need to unmap addresses such as
		// ::ffff:127.0.0.1
		unmapped := addr.Unmap()
		if a == unmapped {
			return true
		}
	}
	for _, p := range a.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (a *addrMatcher) Empty() bool {
	return len(a.addrs) == 0 && len(a.prefixes) == 0
}

// ClientFilter performs allow/deny filtering of client IP addresses
type ClientFilter struct {
	allow *addrMatcher
	deny  *addrMatcher
}

func NewClientFilterAllowAll() *ClientFilter {
	return &ClientFilter{}
}

// NewClientFilter provides a mechanism to evaluate client IP addresses and determine if
// they should be allowed access or not.
// The allows and denies can each or both be nil or netip.ParseAddr allowed values.
func NewClientFilter(allows []string, denies []string) (*ClientFilter, error) {
	allow, err := newAddrMatcher(allows)
	if err != nil {
		return nil, errors.Wrap(err, "invalid allow filter")
	}
	deny, err := newAddrMatcher(denies)
	if err != nil {
		return nil, errors.Wrap(err, "invalid deny filter")
	}
	return &ClientFilter{
		allow: allow,
		deny:  deny,
	}, nil
}

// Allow determines if the given address is allowed by this filter
// where addrStr is a netip.ParseAddr allowed address
func (f *ClientFilter) Allow(addrPort netip.AddrPort) bool {
	if !f.allow.Empty() {
		matched := f.allow.Match(addrPort.Addr())
		return matched
	}
	if !f.deny.Empty() {
		matched := f.deny.Match(addrPort.Addr())
		return !matched
	}

	return true
}
