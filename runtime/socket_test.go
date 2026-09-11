// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"path/filepath"
	"testing"
)

// The socket a consumer hands NewDocker/NewPodman is turned into the client
// host by clientFor as "unix://" + socket. An empty socket would compose the
// bare "unix://" the Docker SDK cannot parse ("unable to parse docker host
// `unix://`"), which is exactly what broke the beacon watch path when its
// config documented "empty uses the runtime default" but core never actually
// defaulted it. These tests pin the contract both constructors now hold:
// empty resolves to the conventional default, non-empty passes through
// verbatim, and the composed host is never a bare "unix://".

// TestNewDockerDefaultsEmptySocket proves NewDocker("") resolves to the
// conventional /var/run/docker.sock, so the composed client host is the
// parseable unix:///var/run/docker.sock rather than a bare unix://.
func TestNewDockerDefaultsEmptySocket(t *testing.T) {
	rt := NewDocker("")
	if rt.socket != defaultDockerSocket {
		t.Fatalf("NewDocker(\"\").socket = %q, want %q", rt.socket, defaultDockerSocket)
	}
	if host := "unix://" + rt.socket; host != "unix:///var/run/docker.sock" {
		t.Fatalf("composed host = %q, want unix:///var/run/docker.sock", host)
	}
}

// TestNewDockerPassesThroughNonEmptySocket proves a non-empty socket is used
// verbatim, unchanged from core's behavior before the empty-socket default.
func TestNewDockerPassesThroughNonEmptySocket(t *testing.T) {
	const custom = "/custom/path/docker.sock"
	rt := NewDocker(custom)
	if rt.socket != custom {
		t.Fatalf("NewDocker(%q).socket = %q, want it unchanged", custom, rt.socket)
	}
}

// TestNewPodmanDefaultsEmptySocket proves NewPodman("") resolves to the
// conventional Podman socket rather than a bare unix://. With XDG_RUNTIME_DIR
// set, the rootless per-user default is deterministic (<dir>/podman/podman.sock),
// which is the branch a consumer running unprivileged actually hits.
func TestNewPodmanDefaultsEmptySocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	rt := NewPodman("")
	want := filepath.Join(dir, "podman", "podman.sock")
	if rt.socket != want {
		t.Fatalf("NewPodman(\"\").socket = %q, want %q", rt.socket, want)
	}
	if rt.socket == "" {
		t.Fatal("NewPodman(\"\") left socket empty, would compose a bare unix://")
	}
}

// TestNewPodmanRootfulFallbackSocket proves the no-XDG_RUNTIME_DIR fallback
// still names the conventional rootful system-service socket for a root
// caller, guarding that the default logic never yields an empty socket.
func TestNewPodmanRootfulFallbackSocket(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	got := defaultPodmanSocket()
	if got == "" {
		t.Fatal("defaultPodmanSocket() returned empty, would compose a bare unix://")
	}
	// A root caller with no XDG_RUNTIME_DIR gets the rootful constant; an
	// unprivileged caller (the test binary's usual case) gets its own
	// /run/user/<uid>/podman/podman.sock. Either way the socket is non-empty
	// and rooted, never bare.
	if !filepath.IsAbs(got) {
		t.Fatalf("defaultPodmanSocket() = %q, want an absolute path", got)
	}
}

// TestNewPodmanPassesThroughNonEmptySocket proves a non-empty socket is used
// verbatim, unchanged.
func TestNewPodmanPassesThroughNonEmptySocket(t *testing.T) {
	const custom = "/custom/path/podman.sock"
	rt := NewPodman(custom)
	if rt.socket != custom {
		t.Fatalf("NewPodman(%q).socket = %q, want it unchanged", custom, rt.socket)
	}
}
