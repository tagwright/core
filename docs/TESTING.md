# Testing and coverage

Coverage is documented honestly, as proven, compile-only, or untested.

## Verified SDK versions

core drives the moby split-out Engine API client, not the frozen
`github.com/docker/docker` monolith. This tag was built and verified against:

- `github.com/moby/moby/client v0.6.0`
- `github.com/moby/moby/api v1.56.0`

Both are normal-semver modules and neither pulls the daemon. The client is
pre-1.0 and has reshaped between v0.x minors, so these versions are pinned
explicitly and recorded here; expect to re-tag core when the client moves. The
`github.com/docker/docker` dependency is gone, and with it the two
unreachable-but-flagged daemon advisories (GO-2026-4887, GO-2026-4883):
`govulncheck ./...` in the go1.25 toolchain reports neither.

Run everything in a golang:1.25 container with `GOTOOLCHAIN=local` and
`GOPRIVATE=github.com/tagwright/*`.

## Docker

Proven, end to end. `go build`, `go vet`, and `go test ./...` run green in
golang:1.25 against the moby client/api versions above. The runtime package's
unit tests, including the mapping tests rewritten against the strongly-typed
api types, run green.

`TestProvisionerDockerRoundTrip` runs against a live Docker socket and drives the
full Provisioner and Runtime surface on the new client: it pulls a tiny image,
creates an isolated (internal) network and a fresh volume, subscribes `Watch`,
creates and starts a container with that volume mounted on that network, asserts
the start event carries the container id, `Inspect`s the running container (state
running, image, the `/data` mount, and a valid IP on the created network),
asserts via `NetworkInspector` that the network really is internal, execs a
probe, pipes a payload through `ExecSpec.Stdin` (the 8-byte StdCopy demux over
the hijacked exec attach) and reads it back, exercises `Kill` and `Stop` on
teardown, and asserts the destroy event after `RemoveContainer`, then tears every
object down. All objects are prefixed `core-itest-` and labelled
`com.tagwright.core.itest=1`, and each is removed by a deferred cleanup
registered the moment it is created, so a mid-test failure still leaves nothing
behind and nothing outside the test's own objects is ever touched. The test skips
cleanly when no Docker socket is reachable, so `go test ./...` stays green on a
host with no daemon. Run it with the socket mounted into the golang container
(`-v /var/run/docker.sock:/var/run/docker.sock`), e.g.
`go test ./runtime -run TestProvisionerDockerRoundTrip -v`.

## Podman

Proven at the decode-and-map contract level (hermetic), and end to end pending a
live socket.

Hermetic. `TestPodmanFixture...` decode captured Podman compat-API JSON
(`/containers/json`, `/containers/{id}/json` including an unbound `"HostIp": ""`
case, and `/networks`) through the api/v1.56.0 types exactly as the client
decodes them on the wire, run them through core's mappings, and assert the
contract, especially the two fields the type reshape touches: the `HostIP`
zero-value mapping to `""` and the `netip.Addr`/`netip.Prefix` IP and subnet
reads. These need no Podman host and run in the offline suite, so they carry the
Podman coverage for those fields today. The fixtures live in
`runtime/testdata/podman_*.json`.

Live, pending a socket. `TestProvisionerPodmanRoundTrip` mirrors the Docker
round-trip on `NewPodman` with the same Watch/Inspect/Kill/Stop coverage, proving
`WithAPIVersionNegotiation` negotiates a working API version against a real
Podman compat socket (including Podman's `remove`->`EventDestroy` compat-event
quirk). It skips cleanly when no Podman compat socket is reachable, so the
offline suite stays green, and is fully written and ready; it has not yet been
run against a live Podman host from the build environment. To run it, reach the
homelab Podman compat socket into the test container (mount it and set
`CONTAINER_HOST=unix:///path`, or mount it at `/run/podman/podman.sock`) and run
`go test ./runtime -run TestProvisionerPodmanRoundTrip -v`.

## Public API stability

The exported surface is unchanged across this migration. A clean-room consumer
(empty module cache, no sibling directory, a `replace` to the local core)
importing `runtime`, constructing `NewDocker`/`NewPodman`, and type-asserting
`NetworkInspector` and `Provisioner` compiles with no code change, and
`go doc -all ./runtime` matches v0.8.0 signature for signature.
