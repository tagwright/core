// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

// These hermetic contract tests (finding 5a part C, acceptance criterion 6a)
// decode captured Podman compat-API JSON through the moby api/v1.56.0 types
// exactly as the client decodes them on the wire, then run the decoded values
// through core's mapping functions and assert the resulting contract. They need
// no Podman host and run in the offline suite.
//
// They carry the Podman coverage for the fields the finding-1 type reshape
// touches: the HostIP zero-value (finding 1a) and the netip.Addr / netip.Prefix
// IP and subnet reads (finding 1b). The moby api types make PortBinding.HostIP a
// netip.Addr, EndpointSettings.IPAddress/GlobalIPv6Address netip.Addr, and
// IPAMConfig.Subnet a netip.Prefix, so the decode itself is the thing under
// test: a compat response with an unbound `"HostIp": ""` must decode to the zero
// Addr and map to "", and real v4/v6 addresses and subnets must decode and map
// through.
//
// The wire shapes mirror what the moby client decodes: /containers/json into a
// []container.Summary, /containers/{id}/json into a container.InspectResponse,
// and /networks into a []network.Summary. Decoding the fixtures into those exact
// types is therefore the same decode path the live client runs.

func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode fixture %s through the api types: %v", name, err)
	}
}

// TestPodmanFixtureContainerInspect is the load-bearing one: it decodes a real
// Podman compat inspect body, including an unbound `"HostIp": ""` port binding,
// and asserts the two finding-1 contracts on the decoded value run through
// core's mappings: the unbound HostIP maps to "" (1a), and the container's v4
// and v6 addresses decode from netip.Addr and are both carried (1b).
func TestPodmanFixtureContainerInspect(t *testing.T) {
	var info container.InspectResponse
	readFixture(t, "podman_container_inspect.json", &info)

	if info.NetworkSettings == nil {
		t.Fatal("inspect fixture decoded with nil NetworkSettings")
	}

	// Ports (finding 1a): the 443/tcp binding carries `"HostIp": ""`, which must
	// decode to the zero netip.Addr and map to the empty string, not the literal
	// "invalid IP". The 80/tcp binding carries an explicit 0.0.0.0.
	ports := mapContainerPorts(info.NetworkSettings.Ports)
	want := []Port{
		{ContainerPort: 80, HostPort: 8080, Protocol: "tcp", HostIP: "0.0.0.0"},
		{ContainerPort: 443, HostPort: 8443, Protocol: "tcp", HostIP: ""},
	}
	if !reflect.DeepEqual(ports, want) {
		t.Fatalf("mapContainerPorts(decoded compat inspect) = %+v,\nwant %+v (unbound HostIp \"\" -> \"\")", ports, want)
	}

	// Networks (finding 1b): the endpoint carries a v4 and a v6 address as
	// netip.Addr; both must decode and be carried, v4 first.
	nets := mapContainerNetworks(info.NetworkSettings.Networks)
	if len(nets) != 1 {
		t.Fatalf("mapContainerNetworks: got %d networks, want 1: %+v", len(nets), nets)
	}
	wantIPs := []netip.Addr{netip.MustParseAddr("10.89.0.4"), netip.MustParseAddr("fd00:89::4")}
	if !reflect.DeepEqual(nets[0].IPs, wantIPs) {
		t.Fatalf("container network IPs = %v, want %v (v4 then v6)", nets[0].IPs, wantIPs)
	}
	if nets[0].Name != "homelab_default" {
		t.Fatalf("container network name = %q, want %q", nets[0].Name, "homelab_default")
	}
}

// TestPodmanFixtureContainerList decodes a real Podman compat container-list
// body and runs each summary's network attachments and mounts through core's
// mappings, proving the list-summary shape (NetworkSettingsSummary, MountPoint)
// decodes and maps. A v6-less endpoint (GlobalIPv6Address "") must carry only
// its v4 address (the zero-value skip from finding 1b).
func TestPodmanFixtureContainerList(t *testing.T) {
	var summaries []container.Summary
	readFixture(t, "podman_containers_list.json", &summaries)

	if len(summaries) != 2 {
		t.Fatalf("decoded %d container summaries, want 2", len(summaries))
	}

	byName := map[string]container.Summary{}
	for _, s := range summaries {
		if len(s.Names) == 0 {
			t.Fatalf("summary %s decoded with no names", s.ID)
		}
		byName[s.Names[0]] = s
	}

	// The v4-only endpoint (GlobalIPv6Address "") must carry exactly its v4
	// address: the empty v6 decodes to the zero Addr and is skipped.
	web := byName["/homelab-web"]
	if web.NetworkSettings == nil {
		t.Fatal("web summary decoded with nil NetworkSettings")
	}
	webNets := mapContainerNetworks(web.NetworkSettings.Networks)
	if len(webNets) != 1 || !reflect.DeepEqual(webNets[0].IPs, []netip.Addr{netip.MustParseAddr("10.89.0.4")}) {
		t.Fatalf("web network IPs = %+v, want a single 10.89.0.4 (empty v6 skipped)", webNets)
	}

	// The mount decodes and maps to a named volume with RW inverted to ReadOnly.
	if len(web.Mounts) != 1 {
		t.Fatalf("web summary decoded %d mounts, want 1", len(web.Mounts))
	}
	gotMount := mapMountPoint(web.Mounts[0])
	wantMount := Mount{
		Type:        MountVolume,
		Name:        "homelab_webdata",
		Source:      "/var/lib/containers/storage/volumes/homelab_webdata/_data",
		Destination: "/usr/share/nginx/html",
		ReadOnly:    false,
	}
	if gotMount != wantMount {
		t.Fatalf("mapMountPoint(decoded compat mount) = %+v, want %+v", gotMount, wantMount)
	}

	// The db endpoint carries both v4 and v6.
	db := byName["/homelab-db"]
	dbNets := mapContainerNetworks(db.NetworkSettings.Networks)
	wantDBIPs := []netip.Addr{netip.MustParseAddr("10.89.0.5"), netip.MustParseAddr("fd00:89::5")}
	if len(dbNets) != 1 || !reflect.DeepEqual(dbNets[0].IPs, wantDBIPs) {
		t.Fatalf("db network IPs = %+v, want %v", dbNets, wantDBIPs)
	}
}

// TestPodmanFixtureNetworksList decodes a real Podman compat /networks body and
// runs each summary through mapNetworkSummary, proving the network.Summary
// shape decodes and that the netip.Prefix subnets (finding 1b) and the Internal
// flag (the isolation compliance fact) map through. A dual-stack network carries
// both its v4 and v6 subnet in IPAM order.
func TestPodmanFixtureNetworksList(t *testing.T) {
	var summaries []network.Summary
	readFixture(t, "podman_networks_list.json", &summaries)

	if len(summaries) != 2 {
		t.Fatalf("decoded %d network summaries, want 2", len(summaries))
	}

	byName := map[string]Network{}
	for _, s := range summaries {
		byName[s.Name] = mapNetworkSummary(s)
	}

	def := byName["homelab_default"]
	wantSubnets := []netip.Prefix{
		netip.MustParsePrefix("10.89.0.0/24"),
		netip.MustParsePrefix("fd00:89::/64"),
	}
	if !reflect.DeepEqual(def.Subnets, wantSubnets) {
		t.Fatalf("homelab_default subnets = %v, want %v (v4 then v6, from netip.Prefix)", def.Subnets, wantSubnets)
	}
	if def.Internal {
		t.Fatalf("homelab_default Internal = true, want false")
	}

	iso := byName["homelab_isolated"]
	if !iso.Internal {
		t.Fatalf("homelab_isolated Internal = false, want true (isolation is a compliance fact)")
	}
	if !reflect.DeepEqual(iso.Subnets, []netip.Prefix{netip.MustParsePrefix("10.89.1.0/24")}) {
		t.Fatalf("homelab_isolated subnets = %v, want a single 10.89.1.0/24", iso.Subnets)
	}
}
