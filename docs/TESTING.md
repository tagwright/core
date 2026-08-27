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

## Podman

Compile-verified only. Both adapters share one request-and-mapping core, and
the Podman adapter carries compile-time assertions that it satisfies `Runtime`
and `NetworkInspector`. None of it has been exercised against a live Podman
socket yet. The Podman-specific surface is limited to the default socket path
and compose-identity label reading, so the shared core behaves identically to
Docker's at compile time. Live Podman validation, including `ListNetworks`
against real Podman networks, is outstanding and needs a Podman host.
