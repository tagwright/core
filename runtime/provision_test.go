// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// itestImage is a tiny image used to stand up a throwaway container. It is
// pulled on demand by the test (skipped if already present) so the test is
// self-contained on a fresh host with network access from the daemon.
const itestImage = "alpine:3.20"

// itestLabel is stamped on every object the integration test creates so a
// human (or a cleanup sweep) can find and remove anything a killed run
// orphaned: `docker ... --filter label=com.tagwright.core.itest=1`.
const itestLabel = "com.tagwright.core.itest"

// dockerSocketForTest returns a Docker API socket path that exists, or "" if
// none is reachable. It honours DOCKER_HOST (unix:// only) and otherwise tries
// the conventional locations. It never dials: existence is enough to decide
// whether to run, and a present-but-dead socket surfaces as a normal error
// once the test starts using it.
func dockerSocketForTest() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		p := strings.TrimPrefix(h, "unix://")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, p := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestProvisionerDockerRoundTrip exercises the full Provisioner and Runtime
// surface against a live Docker socket on the new moby client: pull a tiny
// image, create an isolated network and a fresh volume, subscribe Watch, create
// and start a container with that volume mounted on that network, assert the
// start event carries the container id, Inspect the running container (state,
// image, /data mount, a valid IP on the created network), assert (via
// NetworkInspector) that the network really is internal, exec `true` as a probe,
// pipe a payload into a command's stdin and read it back out, exercise Kill and
// Stop on teardown, and assert the destroy event after RemoveContainer, then
// tear every object down. Every object is prefixed "core-itest-" and labelled,
// and every one is cleaned up via a deferred remove registered the moment it is
// created, so a mid-test failure still leaves nothing behind and nothing outside
// this test's own objects is ever touched.
//
// The test skips cleanly when no Docker socket is reachable from the build
// environment, so `go test ./...` stays green on a host with no daemon.
func TestProvisionerDockerRoundTrip(t *testing.T) {
	socket := dockerSocketForTest()
	if socket == "" {
		t.Skip("no reachable Docker socket (set DOCKER_HOST=unix:///path or mount /var/run/docker.sock); skipping live provisioner test")
	}
	runProvisionerRoundTrip(t, NewDocker(socket), "docker")
}

// runProvisionerRoundTrip is the engine-agnostic body of the live provisioner
// round-trip, shared by the Docker (TestProvisionerDockerRoundTrip) and Podman
// (TestProvisionerPodmanRoundTrip) tests. rt must satisfy Runtime, Provisioner,
// and NetworkInspector; the caller has already confirmed a reachable socket.
func runProvisionerRoundTrip(t *testing.T, rt interface {
	Runtime
	Provisioner
	NetworkInspector
}, engine string) {
	t.Helper()
	t.Cleanup(func() { _ = rt.Close() })

	var prov Provisioner = rt
	var inspector NetworkInspector = rt

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// A short liveness check up front: if the socket file exists but no daemon
	// answers, skip rather than fail, so an environment with a stale socket
	// does not turn into a spurious test failure.
	if _, err := rt.List(ctx); err != nil {
		t.Skipf("%s socket present but not answering (%v); skipping live provisioner test", engine, err)
	}

	suffix := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	netName := "core-itest-net-" + suffix
	volName := "core-itest-vol-" + suffix
	ctrName := "core-itest-ctr-" + suffix
	labels := map[string]string{itestLabel: "1"}

	if err := prov.PullImage(ctx, itestImage); err != nil {
		t.Fatalf("PullImage(%s): %v", itestImage, err)
	}

	netID, err := prov.CreateNetwork(ctx, NetworkSpec{Name: netName, Labels: labels})
	if err != nil {
		t.Fatalf("CreateNetwork(%s): %v", netName, err)
	}
	t.Cleanup(func() {
		if err := prov.RemoveNetwork(context.Background(), netID); err != nil {
			t.Errorf("cleanup RemoveNetwork(%s): %v", netID, err)
		}
	})

	volCreated, err := prov.CreateVolume(ctx, VolumeSpec{Name: volName, Labels: labels})
	if err != nil {
		t.Fatalf("CreateVolume(%s): %v", volName, err)
	}
	if volCreated != volName {
		t.Fatalf("CreateVolume returned name %q, want %q", volCreated, volName)
	}
	t.Cleanup(func() {
		if err := prov.RemoveVolume(context.Background(), volName); err != nil {
			t.Errorf("cleanup RemoveVolume(%s): %v", volName, err)
		}
	})

	// Subscribe Watch BEFORE the container is created and started, so the start
	// event cannot be missed to a race. The watch runs on its own cancellable
	// context that outlives the container's create/inspect/teardown below, so
	// the destroy event after RemoveContainer is observed too.
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	events, watchErrs := rt.Watch(watchCtx)

	// ctrRemoved guards the safety-net cleanup: the container is torn down
	// explicitly in-body (to observe the destroy event while the watch is live),
	// so the deferred force-remove must not then fail on an already-gone id.
	var ctrRemoved bool
	ctrID, err := prov.CreateContainer(ctx, ContainerSpec{
		Name:    ctrName,
		Image:   itestImage,
		Cmd:     []string{"sleep", "300"},
		Labels:  labels,
		Mounts:  []VolumeMount{{Volume: volName, Destination: "/data"}},
		Network: netName,
		Start:   true,
	})
	if ctrID != "" {
		// Register teardown even on a start error, since CreateContainer
		// returns the id of a created-but-unstarted container.
		t.Cleanup(func() {
			if ctrRemoved {
				return
			}
			if err := prov.RemoveContainer(context.Background(), ctrID, true); err != nil {
				t.Errorf("cleanup RemoveContainer(%s): %v", ctrID, err)
			}
		})
	}
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", ctrName, err)
	}

	// The start event must arrive on Watch carrying the container id (finding 3).
	waitForEvent(t, events, watchErrs, EventStart, ctrID, 30*time.Second)

	// Inspect the running container and assert the fields a consumer reads off
	// it: State running, the image, the /data mount, and a valid IP on the
	// created network (finding 3).
	inspected, err := rt.Inspect(ctx, ctrID)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", ctrID, err)
	}
	if inspected.State != "running" {
		t.Fatalf("Inspect: State = %q, want running", inspected.State)
	}
	if inspected.Image == "" {
		t.Fatalf("Inspect: Image is empty, want the created image reference")
	}
	if !hasMountAt(inspected.Mounts, "/data") {
		t.Fatalf("Inspect: no mount at /data, got %+v", inspected.Mounts)
	}
	if !hasValidIPOnNetwork(inspected.Networks, netName) {
		t.Fatalf("Inspect: no valid IP on network %s, got %+v", netName, inspected.Networks)
	}

	// The isolation of the network is the compliance fact verify leans on, so
	// assert it directly: the network the container is attached to reports
	// Internal == true.
	assertNetworkInternal(ctx, t, inspector, netName)

	// Probe: exec `true` and confirm a clean exit and empty output.
	probe, err := rt.Exec(ctx, ctrID, ExecSpec{Cmd: []string{"true"}})
	if err != nil {
		t.Fatalf("Exec(true): %v", err)
	}
	out, _ := io.ReadAll(probe.Stdout)
	if code, err := probe.Wait(); err != nil || code != 0 {
		t.Fatalf("Exec(true): code=%d err=%v", code, err)
	}
	if len(out) != 0 {
		t.Fatalf("Exec(true): unexpected stdout %q", out)
	}

	// Stream-restore path: pipe a payload into a command's stdin, then read it
	// back to prove ExecSpec.Stdin is wired through end to end.
	payload := "billet-verify-stream-restore\n"
	writer, err := rt.Exec(ctx, ctrID, ExecSpec{
		Cmd:   []string{"sh", "-c", "cat > /data/restored"},
		Stdin: strings.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("Exec(stdin write): %v", err)
	}
	_, _ = io.ReadAll(writer.Stdout)
	if code, err := writer.Wait(); err != nil || code != 0 {
		t.Fatalf("Exec(stdin write): code=%d err=%v", code, err)
	}

	reader, err := rt.Exec(ctx, ctrID, ExecSpec{Cmd: []string{"cat", "/data/restored"}})
	if err != nil {
		t.Fatalf("Exec(read back): %v", err)
	}
	got, err := io.ReadAll(reader.Stdout)
	if err != nil {
		t.Fatalf("read back stdout: %v", err)
	}
	if code, err := reader.Wait(); err != nil || code != 0 {
		t.Fatalf("Exec(read back): code=%d err=%v", code, err)
	}
	if string(got) != payload {
		t.Fatalf("stdin round-trip: got %q, want %q", got, payload)
	}

	// Teardown exercises Kill and Stop rather than leaning only on force-remove
	// (finding 3). Kill delivers a harmless SIGCONT to the still-running
	// container to prove the Kill path against the new client, then Stop stops it
	// gracefully; only then is it removed with force=false, which the daemon
	// answers with a destroy event.
	if err := rt.Kill(ctx, ctrID, "SIGCONT"); err != nil {
		t.Fatalf("Kill(SIGCONT): %v", err)
	}
	if err := rt.Stop(ctx, ctrID, 10); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := prov.RemoveContainer(ctx, ctrID, false); err != nil {
		t.Fatalf("RemoveContainer(force=false): %v", err)
	}
	ctrRemoved = true

	// The destroy event must arrive after removal (finding 3). Podman's compat
	// API emits "remove" where Docker emits "destroy"; both normalize to
	// EventDestroy, so this assertion holds on either engine.
	waitForEvent(t, events, watchErrs, EventDestroy, ctrID, 30*time.Second)
}

// waitForEvent blocks until an event of the wanted type and container id arrives
// on the watch stream, failing the test on the watch error channel or on
// timeout. Events for other containers (a busy host) are ignored.
func waitForEvent(t *testing.T, events <-chan Event, watchErrs <-chan error, want EventType, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("watch event channel closed before a %q event for %s arrived", want, id)
			}
			if ev.Type == want && ev.ID == id {
				return
			}
		case err := <-watchErrs:
			if err != nil {
				t.Fatalf("watch error while waiting for %q on %s: %v", want, id, err)
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for a %q event for %s", timeout, want, id)
		}
	}
}

// hasMountAt reports whether the mounts include one at the given destination.
func hasMountAt(mounts []Mount, dest string) bool {
	for _, m := range mounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}

// hasValidIPOnNetwork reports whether the container holds at least one valid IP
// on the named network.
func hasValidIPOnNetwork(nets []ContainerNetwork, name string) bool {
	for _, n := range nets {
		if n.Name != name {
			continue
		}
		for _, ip := range n.IPs {
			if ip.IsValid() {
				return true
			}
		}
	}
	return false
}

// assertNetworkInternal fails the test unless the named network is present in
// the inspector's inventory and marked internal.
func assertNetworkInternal(ctx context.Context, t *testing.T, inspector NetworkInspector, name string) {
	t.Helper()
	nets, err := inspector.ListNetworks(ctx)
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	for _, n := range nets {
		if n.Name == name {
			if !n.Internal {
				t.Fatalf("network %s: Internal=false, want true (isolation is a compliance fact)", name)
			}
			return
		}
	}
	t.Fatalf("network %s not found in ListNetworks inventory", name)
}
