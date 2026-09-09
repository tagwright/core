// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import "testing"

// The compose-identity functions decide which compose project and service a
// container belongs to, and a consumer groups, targets, and reports actions by
// that identity. A misread would attribute a container to the wrong service,
// so both the Docker mapping and the Podman precedence-plus-fallback mapping
// are locked here by invariant.
//
// Closes Vikunja #650 (compose-identity half).

// TestDockerComposeIdentity proves the Docker mapping reads exactly the
// com.docker.compose.* pair and nothing else. A container with neither label
// (one not brought up by compose) must yield empty project and service, which
// is the unstacked signal the caller reads as not-a-compose-service, and a
// container carrying only one of the two labels must not borrow a value for
// the missing one.
func TestDockerComposeIdentity(t *testing.T) {
	cases := []struct {
		name        string
		labels      map[string]string
		wantProject string
		wantService string
	}{
		{
			name: "both present",
			labels: map[string]string{
				composeProjectLabel: "homelab",
				composeServiceLabel: "postgres",
			},
			wantProject: "homelab",
			wantService: "postgres",
		},
		{
			name:        "neither present is unstacked",
			labels:      map[string]string{"unrelated": "value"},
			wantProject: "",
			wantService: "",
		},
		{
			name:        "nil labels is unstacked",
			labels:      nil,
			wantProject: "",
			wantService: "",
		},
		{
			name:        "only project present",
			labels:      map[string]string{composeProjectLabel: "homelab"},
			wantProject: "homelab",
			wantService: "",
		},
		{
			name:        "only service present",
			labels:      map[string]string{composeServiceLabel: "postgres"},
			wantProject: "",
			wantService: "postgres",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			project, service := dockerComposeIdentity(c.labels)
			if project != c.wantProject || service != c.wantService {
				t.Fatalf("dockerComposeIdentity(%v) = (%q, %q), want (%q, %q)",
					c.labels, project, service, c.wantProject, c.wantService)
			}
		})
	}
}

// TestPodmanComposeIdentity exercises the fallback branch that has never been
// covered: podmanComposeIdentity prefers the Docker-compatible
// com.docker.compose.* pair and falls back to the Podman-native
// io.podman.compose.* pair only when the compat labels are absent. The
// decisive field is the compat service label, so the cases pin the full
// precedence: compat wins when set even if the Podman pair is also present,
// the Podman pair is used only when the compat service is absent, and the
// fallback returns both Podman values together rather than mixing a compat
// project with a Podman service.
func TestPodmanComposeIdentity(t *testing.T) {
	cases := []struct {
		name        string
		labels      map[string]string
		wantProject string
		wantService string
	}{
		{
			// Both pairs present with different values: the compat pair wins.
			name: "compat present wins over podman-native",
			labels: map[string]string{
				composeProjectLabel:       "compat-project",
				composeServiceLabel:       "compat-service",
				podmanComposeProjectLabel: "podman-project",
				podmanComposeServiceLabel: "podman-service",
			},
			wantProject: "compat-project",
			wantService: "compat-service",
		},
		{
			// Only the Podman-native pair present: the fallback branch fires.
			name: "podman-native only uses fallback",
			labels: map[string]string{
				podmanComposeProjectLabel: "podman-project",
				podmanComposeServiceLabel: "podman-service",
			},
			wantProject: "podman-project",
			wantService: "podman-service",
		},
		{
			// The branch keys off the compat service label, not the compat
			// project. With the compat service absent, the whole Podman pair
			// is returned, and the stray compat project must not be mixed in.
			name: "compat project without compat service falls back wholesale",
			labels: map[string]string{
				composeProjectLabel:       "compat-project",
				podmanComposeProjectLabel: "podman-project",
				podmanComposeServiceLabel: "podman-service",
			},
			wantProject: "podman-project",
			wantService: "podman-service",
		},
		{
			name:        "neither pair present is unstacked",
			labels:      map[string]string{"unrelated": "value"},
			wantProject: "",
			wantService: "",
		},
		{
			name:        "nil labels is unstacked",
			labels:      nil,
			wantProject: "",
			wantService: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			project, service := podmanComposeIdentity(c.labels)
			if project != c.wantProject || service != c.wantService {
				t.Fatalf("podmanComposeIdentity(%v) = (%q, %q), want (%q, %q)",
					c.labels, project, service, c.wantProject, c.wantService)
			}
		})
	}
}
