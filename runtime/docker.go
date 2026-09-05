// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

// Compose label keys Docker attaches to a container that a compose file
// brought up. Their presence (and value) is how a consumer recovers the
// project/service grouping without parsing any compose YAML itself.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
)

// DockerRuntime is the Docker adapter for Runtime. It talks to the Docker
// Engine API over the socket a consumer mounts read-only, using the request and
// mapping machinery in engine.go that it shares with PodmanRuntime.
//
// The client is created lazily on first use and cached, so constructing a
// DockerRuntime never touches the socket: nothing fails until a method that
// actually needs the daemon is called.
type DockerRuntime struct {
	*engineClient
}

// NewDocker returns a Docker adapter bound to the given API socket path.
func NewDocker(socket string) *DockerRuntime {
	return &DockerRuntime{engineClient: &engineClient{
		engine:   "docker",
		socket:   socket,
		identity: dockerComposeIdentity,
	}}
}

// compile-time assertion that the adapter satisfies the interface.
var _ Runtime = (*DockerRuntime)(nil)

// compile-time assertion that the adapter also satisfies the optional
// NetworkInspector capability, via engineClient.ListNetworks.
var _ NetworkInspector = (*DockerRuntime)(nil)

// compile-time assertion that the adapter also satisfies the optional
// Provisioner capability, via the create/remove methods on engineClient
// (provision.go). This is the surface a consumer's verify or restore flow
// drives to stand up and tear down a throwaway restore sandbox.
var _ Provisioner = (*DockerRuntime)(nil)

// dockerComposeIdentity resolves compose project/service names from
// Docker's own com.docker.compose.* labels.
func dockerComposeIdentity(labels map[string]string) (project, service string) {
	return labels[composeProjectLabel], labels[composeServiceLabel]
}
