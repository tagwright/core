// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// mapContainerPorts decides which host port bindings a container is reported to
// publish, and a consumer deciding whether a service is reachable from off-host
// acts on exactly that set. A dropped or reordered binding would hide a
// published surface or churn a regenerate-and-compare consumer's output, so the
// mapping is locked here by invariant: every host binding appears once, the
// container/host ports and protocol are read off the inspect key and binding,
// exposed-but-unpublished ports contribute nothing, the order is deterministic,
// and the empty case is nil.
//
// The mapping is shared by the Docker and Podman adapters (both embed the same
// engineClient and Podman's compat API reports NetworkSettings.Ports in the
// identical shape), so proving it here proves it for both engines.

// TestMapContainerPortsEmpty proves both a nil map and an empty map map to a nil
// slice, the shape Inspect stores on a container that publishes nothing. A
// caller ranges over the result either way, so nil is the correct empty rather
// than a zero-length allocation.
func TestMapContainerPortsEmpty(t *testing.T) {
	if got := mapContainerPorts(nil); got != nil {
		t.Fatalf("mapContainerPorts(nil) = %+v, want nil", got)
	}
	if got := mapContainerPorts(network.PortMap{}); got != nil {
		t.Fatalf("mapContainerPorts(empty) = %+v, want nil", got)
	}
}

// TestMapContainerPortsUnpublishedSkipped proves a port that is exposed by the
// image but not published to the host (its key is present with a nil or empty
// binding list) contributes no Port, while a sibling published port on the same
// container still comes through. This is the reachability invariant: only a real
// host binding counts as a published port.
func TestMapContainerPortsUnpublishedSkipped(t *testing.T) {
	in := network.PortMap{
		network.MustParsePort("5432/tcp"): nil,                     // exposed, not published
		network.MustParsePort("9000/tcp"): []network.PortBinding{}, // published to nothing
		network.MustParsePort("80/tcp"): []network.PortBinding{
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}, // published
		},
	}
	got := mapContainerPorts(in)
	want := []Port{{ContainerPort: 80, HostPort: 8080, Protocol: "tcp", HostIP: "0.0.0.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapContainerPorts = %+v, want %+v (only the published binding survives)", got, want)
	}
}

// TestMapContainerPortsMultiBindingSorted proves every host binding survives the
// mapping, that the container/host ports and protocol are read off the key and
// binding, and that the result is sorted deterministically (by container port,
// then protocol, host IP, host port). The input is keyed out of sorted order and
// carries a dual-stack binding (a port published to both an IPv4 and an IPv6 host
// address, two bindings under one key), so a mapper that returned map order, or
// collapsed the dual binding, would fail.
func TestMapContainerPortsMultiBindingSorted(t *testing.T) {
	in := network.PortMap{
		network.MustParsePort("443/tcp"): []network.PortBinding{
			{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "443"},
			{HostIP: netip.MustParseAddr("::"), HostPort: "443"},
		},
		network.MustParsePort("53/udp"): []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "53"}},
		network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "8080"}},
	}
	got := mapContainerPorts(in)
	want := []Port{
		{ContainerPort: 53, HostPort: 53, Protocol: "udp", HostIP: "127.0.0.1"},
		{ContainerPort: 80, HostPort: 8080, Protocol: "tcp", HostIP: "0.0.0.0"},
		{ContainerPort: 443, HostPort: 443, Protocol: "tcp", HostIP: "0.0.0.0"},
		{ContainerPort: 443, HostPort: 443, Protocol: "tcp", HostIP: "::"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapContainerPorts = %+v,\nwant %+v (all bindings present, sorted)", got, want)
	}
}

// TestMapContainerPortsUnparseableHostPort proves a host-port string that does
// not parse as a number is carried as HostPort 0 rather than failing or dropping
// the whole binding: the container port and host IP still come through, so a
// consumer sees the binding exists even when the port string is malformed. This
// does not happen for a well-formed engine response, but the mapping must not
// panic or lose the entry if it does.
func TestMapContainerPortsUnparseableHostPort(t *testing.T) {
	in := network.PortMap{
		network.MustParsePort("80/tcp"): []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "not-a-number"}},
	}
	got := mapContainerPorts(in)
	want := []Port{{ContainerPort: 80, HostPort: 0, Protocol: "tcp", HostIP: "0.0.0.0"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapContainerPorts = %+v, want %+v (unparseable host port -> 0, binding kept)", got, want)
	}
}

// TestMapContainerPortsZeroHostIP proves a binding with a zero (unbound) HostIP
// maps to the empty string, not the literal "invalid IP" that
// netip.Addr{}.String() returns. This is finding 1a: in the moby api types
// PortBinding.HostIP is a netip.Addr, so an unbound binding carries the zero
// Addr, and runtime.go's Port contract promises "" for HostIP when the engine
// reports none. A consumer classifying off-host reachability keys on HostIP, so
// a zero address mis-mapped to a non-empty string would read as reachable.
// Asserted offline here, independent of the live round-trip.
func TestMapContainerPortsZeroHostIP(t *testing.T) {
	in := network.PortMap{
		// HostPort set but HostIP left as the zero Addr: an unbound host address.
		network.MustParsePort("80/tcp"): []network.PortBinding{{HostPort: "8080"}},
	}
	got := mapContainerPorts(in)
	want := []Port{{ContainerPort: 80, HostPort: 8080, Protocol: "tcp", HostIP: ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapContainerPorts = %+v, want %+v (zero HostIP -> \"\", not \"invalid IP\")", got, want)
	}
}

// restartPolicyName is the field a consumer reads to tell a long-running service
// (a policy of always/unless-stopped) from a one-shot job, so the mapping is
// locked here: the configured name is read off HostConfig, and a nil HostConfig
// (the shape the engine reports for a container with no policy, or a partial
// response) yields "" rather than a panic. Shared by both adapters, so this
// covers Docker and Podman alike.

// TestRestartPolicyNameNil proves a nil HostConfig maps to the empty string,
// which is the guard that keeps Inspect from panicking on a container whose
// inspect carries no HostConfig.
func TestRestartPolicyNameNil(t *testing.T) {
	if got := restartPolicyName(nil); got != "" {
		t.Fatalf("restartPolicyName(nil) = %q, want empty", got)
	}
}

// TestRestartPolicyNameValues proves each restart-policy mode the engine reports
// is carried through verbatim as its string, including the empty (unset) policy.
func TestRestartPolicyNameValues(t *testing.T) {
	cases := []struct {
		mode container.RestartPolicyMode
		want string
	}{
		{container.RestartPolicyDisabled, "no"},
		{container.RestartPolicyAlways, "always"},
		{container.RestartPolicyUnlessStopped, "unless-stopped"},
		{container.RestartPolicyOnFailure, "on-failure"},
		{"", ""},
	}
	for _, tc := range cases {
		hc := &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: tc.mode}}
		if got := restartPolicyName(hc); got != tc.want {
			t.Fatalf("restartPolicyName(%q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}
