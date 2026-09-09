// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/network"
)

// mapContainerNetworks decides which networks a container is reported to be
// attached to, and a consumer that certifies egress isolation acts on exactly
// that set. A dropped attachment would hide a network a container really sits
// on, so the mapping is locked here by invariant: every non-nil attachment
// appears, the order is deterministic, and the empty case is nil.
//
// Closes Vikunja #512.

// TestMapContainerNetworksEmpty proves both the nil map and the empty map map
// to a nil slice, which is the shape List and Inspect store on a container
// with no reported network settings. A caller ranges over the result either
// way, so nil is the correct empty rather than a zero-length allocation.
func TestMapContainerNetworksEmpty(t *testing.T) {
	if got := mapContainerNetworks(nil); got != nil {
		t.Fatalf("mapContainerNetworks(nil) = %+v, want nil", got)
	}
	if got := mapContainerNetworks(map[string]*network.EndpointSettings{}); got != nil {
		t.Fatalf("mapContainerNetworks(empty) = %+v, want nil", got)
	}
}

// TestMapContainerNetworksAllPresentAndSorted proves every attached network
// name survives the mapping and that the result is sorted by name for a
// deterministic order, since the engine's map iteration is not. The input is
// deliberately keyed out of sorted order so a mapper that returned map order
// would fail.
func TestMapContainerNetworksAllPresentAndSorted(t *testing.T) {
	in := map[string]*network.EndpointSettings{
		"zeta":    {NetworkID: "id-zeta"},
		"alpha":   {NetworkID: "id-alpha"},
		"mid-net": {NetworkID: "id-mid"},
	}
	got := mapContainerNetworks(in)

	wantNames := []string{"alpha", "mid-net", "zeta"}
	if len(got) != len(wantNames) {
		t.Fatalf("mapContainerNetworks: got %d networks, want %d (%+v)", len(got), len(wantNames), got)
	}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Fatalf("mapContainerNetworks: position %d = %q, want %q (result not sorted by name: %+v)", i, got[i].Name, name, got)
		}
	}
	// The network id must be carried alongside the name, since a consumer
	// correlates a container's attachment to the network inventory by id.
	byName := map[string]ContainerNetwork{}
	for _, cn := range got {
		byName[cn.Name] = cn
	}
	if byName["alpha"].ID != "id-alpha" {
		t.Fatalf("mapContainerNetworks: alpha.ID = %q, want %q", byName["alpha"].ID, "id-alpha")
	}
}

// TestMapContainerNetworksNilEndpointSkipped proves a nil endpoint entry (a
// network key present with no settings) is skipped rather than panicking or
// emitting a zero-value attachment, while the sibling real attachments still
// come through.
func TestMapContainerNetworksNilEndpointSkipped(t *testing.T) {
	in := map[string]*network.EndpointSettings{
		"real": {NetworkID: "id-real"},
		"nil":  nil,
	}
	got := mapContainerNetworks(in)
	if len(got) != 1 {
		t.Fatalf("mapContainerNetworks: got %d networks, want 1 (nil endpoint should be skipped): %+v", len(got), got)
	}
	if got[0].Name != "real" {
		t.Fatalf("mapContainerNetworks: surviving network = %q, want %q", got[0].Name, "real")
	}
}

// TestMapContainerNetworksIPs proves the address handling invariant: a
// parseable IPv4 and a parseable IPv6 are both carried (IPv4 first, then
// IPv6, matching the mapper's read order), while an empty or unparseable
// address string is skipped rather than failing the whole attachment. An
// empty IPv6 address is the common real case for a container that holds no
// address on that family.
func TestMapContainerNetworksIPs(t *testing.T) {
	in := map[string]*network.EndpointSettings{
		"dual": {
			NetworkID:         "id-dual",
			IPAddress:         "172.31.0.6",
			GlobalIPv6Address: "fd00::6",
		},
		"v4only": {
			NetworkID:         "id-v4only",
			IPAddress:         "10.0.0.2",
			GlobalIPv6Address: "",
		},
		"noaddr": {
			NetworkID:         "id-noaddr",
			IPAddress:         "",
			GlobalIPv6Address: "",
		},
	}
	got := mapContainerNetworks(in)

	byName := map[string]ContainerNetwork{}
	for _, cn := range got {
		byName[cn.Name] = cn
	}

	wantDual := []netip.Addr{netip.MustParseAddr("172.31.0.6"), netip.MustParseAddr("fd00::6")}
	if !reflect.DeepEqual(byName["dual"].IPs, wantDual) {
		t.Fatalf("dual.IPs = %v, want %v (IPv4 first, then IPv6)", byName["dual"].IPs, wantDual)
	}

	wantV4 := []netip.Addr{netip.MustParseAddr("10.0.0.2")}
	if !reflect.DeepEqual(byName["v4only"].IPs, wantV4) {
		t.Fatalf("v4only.IPs = %v, want %v (empty IPv6 must be skipped)", byName["v4only"].IPs, wantV4)
	}

	if len(byName["noaddr"].IPs) != 0 {
		t.Fatalf("noaddr.IPs = %v, want empty (no parseable address)", byName["noaddr"].IPs)
	}
}
