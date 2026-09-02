# Testing and coverage

Coverage is documented honestly, as proven, compile-only, or untested.

## Docker

Proven. `go build`, `go vet`, and `go test ./...` run green in golang:1.25
against the docker/docker v28.5.2 SDK. The runtime package's unit tests run
green.

Network introspection (`NetworkInspector.ListNetworks` and
`Container.Networks`) is Docker-proven at the mapping level: `network.Summary`
is a type alias for `network.Inspect` in the pinned SDK, so `NetworkList`
returns the full IPAM config, driver, internal flag, and labels in one call,
with no per-network inspect. Live end-to-end exercise against a running Docker
socket is not yet part of the test suite.

Provisioning (`Provisioner` and the optional `ExecSpec.Stdin`) is Docker-proven
end to end. `TestProvisionerDockerRoundTrip` runs against a live Docker socket:
it pulls a tiny image, creates an isolated (internal) network and a fresh
volume, creates and starts a container with that volume mounted on that
network, asserts via `NetworkInspector` that the network really is internal,
execs a probe, pipes a payload through `ExecSpec.Stdin` and reads it back, then
tears every object down. All objects are prefixed `core-itest-` and labelled
`com.tagwright.core.itest=1`, and each is removed by a deferred cleanup
registered the moment it is created, so a mid-test failure still leaves nothing
behind and nothing outside the test's own objects is ever touched. The test
skips cleanly when no Docker socket is reachable, so `go test ./...` stays green
on a host with no daemon. Run it with the socket mounted into the golang
container (`-v /var/run/docker.sock:/var/run/docker.sock`).

## Podman

Compile-verified only. Both adapters share one request-and-mapping core, and
the Podman adapter carries compile-time assertions that it satisfies `Runtime`
and `NetworkInspector`. None of it has been exercised against a live Podman
socket yet. The Podman-specific surface is limited to the default socket path
and compose-identity label reading, so the shared core behaves identically to
Docker's at compile time. Live Podman validation, including `ListNetworks`
against real Podman networks, is outstanding and needs a Podman host.
