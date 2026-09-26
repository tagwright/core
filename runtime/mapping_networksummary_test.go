// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/network"
)

// mapNetworkSummary produces the normalized Network a consumer's egress
// classification and isolation certification read. The Internal flag is the
// compliance fact (a network marked internal cannot reach outside the host),
// so a mapping that dropped or flipped it would silently certify an exposed
// network as isolated. The flag and the subnet parsing are locked here by
// invariant.
//
// Closes Vikunja #650 (network-summary half).

// TestMapNetworkSummaryInternalFlag proves the Internal flag is carried
// faithfully in both directions. This is the isolation fact, so both an
// internal network and a non-internal one are asserted rather than only the
// isolated case.
func TestMapNetworkSummaryInternalFlag(t *testing.T) {
	cases := []struct {
		name     string
		internal bool
	}{
		{"internal network", true},
		{"non-internal network", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapNetworkSummary(network.Summary{Network: network.Network{Name: c.name, Internal: c.internal}})
			if got.Internal != c.internal {
				t.Fatalf("mapNetworkSummary(Internal=%v).Internal = %v, want %v", c.internal, got.Internal, c.internal)
			}
		})
	}
}

// TestMapNetworkSummaryScalarFields proves the fields the mapper copies
// verbatim (Name, ID, Driver, Labels) are carried, so a consumer that
// correlates a container's attachment to a network by id, or classifies by
// driver, or reads a policy label, sees the engine's own values.
func TestMapNetworkSummaryScalarFields(t *testing.T) {
	labels := map[string]string{"com.tagwright.egress": "deny"}
	in := network.Summary{Network: network.Network{
		Name:     "app-internal",
		ID:       "net-id-1234",
		Driver:   "bridge",
		Internal: true,
		Labels:   labels,
	}}
	got := mapNetworkSummary(in)

	if got.Name != in.Name {
		t.Fatalf("Name = %q, want %q", got.Name, in.Name)
	}
	if got.ID != in.ID {
		t.Fatalf("ID = %q, want %q", got.ID, in.ID)
	}
	if got.Driver != in.Driver {
		t.Fatalf("Driver = %q, want %q", got.Driver, in.Driver)
	}
	if !reflect.DeepEqual(got.Labels, labels) {
		t.Fatalf("Labels = %v, want %v", got.Labels, labels)
	}
}

// TestMapNetworkSummarySubnets proves the subnet parsing invariant: a well
// formed IPv4 CIDR and a well formed IPv6 CIDR are both carried as
// netip.Prefix in IPAM order, while a zero-value (unset) subnet is skipped
// rather than emitting an invalid Prefix. A valid entry must not be hidden by a
// sibling zero entry, so a caller doing egress classification still sees every
// real subnet.
//
// In the moby api types IPAMConfig.Subnet is already a netip.Prefix, so the old
// deliberately-unparseable "not-a-cidr" string input can no longer be
// expressed: a malformed subnet now fails JSON decoding of the whole
// NetworkList response inside the client, before mapNetworkSummary runs (see
// the finding 1b note in runtime.go). Per finding 2 that case is replaced by an
// explicit zero-value entry carrying the same "must be skipped" intent, not by
// relaxing the expectation.
func TestMapNetworkSummarySubnets(t *testing.T) {
	in := network.Summary{Network: network.Network{
		Name: "mixed",
		IPAM: network.IPAM{
			Config: []network.IPAMConfig{
				{Subnet: netip.MustParsePrefix("172.31.0.0/16")}, // carried
				{},                       // skipped: zero-value (unset) subnet
				{Subnet: netip.Prefix{}}, // skipped: explicit zero-value subnet
				{Subnet: netip.MustParsePrefix("fd00::/64")}, // carried
			},
		},
	}}
	got := mapNetworkSummary(in)

	want := []netip.Prefix{
		netip.MustParsePrefix("172.31.0.0/16"),
		netip.MustParsePrefix("fd00::/64"),
	}
	if !reflect.DeepEqual(got.Subnets, want) {
		t.Fatalf("Subnets = %v, want %v (zero-value entries must be skipped, order preserved)", got.Subnets, want)
	}
}

// TestMapNetworkSummaryNoSubnets proves a network with no IPAM config yields
// an empty subnet slice rather than a panic, which is the common case for a
// network the engine reports without an address pool.
func TestMapNetworkSummaryNoSubnets(t *testing.T) {
	got := mapNetworkSummary(network.Summary{Network: network.Network{Name: "poolless"}})
	if len(got.Subnets) != 0 {
		t.Fatalf("Subnets = %v, want empty", got.Subnets)
	}
}
