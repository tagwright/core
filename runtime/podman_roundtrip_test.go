// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// podmanSocketForTest returns a Podman compat API socket path that exists, or ""
// if none is reachable. It honours CONTAINER_HOST and DOCKER_HOST (unix:// only)
// and otherwise tries the conventional rootful and rootless locations. It never
// dials: existence is enough to decide whether to run, and a present-but-dead
// socket surfaces as a normal error (or a clean skip via the liveness check)
// once the test starts using it.
func podmanSocketForTest() string {
	for _, env := range []string{"CONTAINER_HOST", "DOCKER_HOST"} {
		if h := os.Getenv(env); strings.HasPrefix(h, "unix://") {
			p := strings.TrimPrefix(h, "unix://")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}

	candidates := []string{"/run/podman/podman.sock", "/var/run/podman/podman.sock"}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		candidates = append(candidates, dir+"/podman/podman.sock")
	}
	candidates = append(candidates, fmt.Sprintf("/run/user/%d/podman/podman.sock", os.Getuid()))

	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestProvisionerPodmanRoundTrip is the live Podman counterpart of
// TestProvisionerDockerRoundTrip (acceptance criterion 6b, finding 5a part D).
// It runs the identical Provisioner/Runtime round-trip through NewPodman against
// a real Podman compat socket, with the same Watch/Inspect/Kill/Stop coverage,
// proving that WithAPIVersionNegotiation negotiates a working API version
// against Podman and that the shared engineClient behaves the same on both
// engines (including Podman's "remove"->EventDestroy compat-event quirk, which
// the shared mapping and the destroy-event assertion in the round-trip cover).
//
// It skips cleanly when no Podman compat socket is reachable, so `go test ./...`
// stays green on a host with no Podman. The criterion is met only when this has
// actually run green against a real Podman host: reach the homelab Podman socket
// into the test container (mount it and point CONTAINER_HOST at it, or mount it
// at /run/podman/podman.sock) and run
// `go test ./runtime -run TestProvisionerPodmanRoundTrip -v`.
func TestProvisionerPodmanRoundTrip(t *testing.T) {
	socket := podmanSocketForTest()
	if socket == "" {
		t.Skip("no reachable Podman compat socket (set CONTAINER_HOST=unix:///path or mount /run/podman/podman.sock); skipping live Podman provisioner test")
	}
	runProvisionerRoundTrip(t, NewPodman(socket), "podman")
}
