// SPDX-License-Identifier: GPL-3.0-or-later

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

// TestProvisionerDockerRoundTrip exercises the full Provisioner surface against
// a live Docker socket: pull a tiny image, create an isolated network and a
// fresh volume, create and start a container with that volume mounted on that
// network, assert (via NetworkInspector) that the network really is internal,
// exec `true` as a probe, pipe a payload into a command's stdin and read it
// back out, then tear every object down. Every object is prefixed
// "core-itest-" and labelled, and every one is cleaned up via a deferred
// remove registered the moment it is created, so a mid-test failure still
// leaves nothing behind and nothing outside this test's own objects is ever
// touched.
//
// The test skips cleanly when no Docker socket is reachable from the build
// environment, so `go test ./...` stays green on a host with no daemon.
func TestProvisionerDockerRoundTrip(t *testing.T) {
	socket := dockerSocketForTest()
	if socket == "" {
		t.Skip("no reachable Docker socket (set DOCKER_HOST=unix:///path or mount /var/run/docker.sock); skipping live provisioner test")
	}

	rt := NewDocker(socket)
	t.Cleanup(func() { _ = rt.Close() })

	var prov Provisioner = rt // compile-time and runtime proof DockerRuntime satisfies Provisioner
	inspector, ok := any(rt).(NetworkInspector)
	if !ok {
		t.Fatal("DockerRuntime does not satisfy NetworkInspector")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// A short liveness check up front: if the socket file exists but no daemon
	// answers, skip rather than fail, so an environment with a stale socket
	// does not turn into a spurious test failure.
	if _, err := rt.List(ctx); err != nil {
		t.Skipf("Docker socket %s present but not answering (%v); skipping live provisioner test", socket, err)
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
			if err := prov.RemoveContainer(context.Background(), ctrID, true); err != nil {
				t.Errorf("cleanup RemoveContainer(%s): %v", ctrID, err)
			}
		})
	}
	if err != nil {
		t.Fatalf("CreateContainer(%s): %v", ctrName, err)
	}

	// The isolation of the network is the compliance fact verify leans on, so
	// assert it directly: the network the container is attached to reports
	// Internal == true.
	assertNetworkInternal(ctx, t, inspector, netName)

	// Probe: exec `true` and confirm a clean exit and empty output.
	probe, err := prov.(Runtime).Exec(ctx, ctrID, ExecSpec{Cmd: []string{"true"}})
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
	writer, err := prov.(Runtime).Exec(ctx, ctrID, ExecSpec{
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

	reader, err := prov.(Runtime).Exec(ctx, ctrID, ExecSpec{Cmd: []string{"cat", "/data/restored"}})
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
