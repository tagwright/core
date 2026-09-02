# tagwright/core

Shared building blocks for the tagwright suite. The first piece is `runtime`, a
container-runtime abstraction over Docker and Podman. It talks to a Docker Engine
API-compatible socket and exposes the operations the suite's tools need: list and
inspect containers, watch the socket for lifecycle events, read compose
project/service labels, stop and start containers, exec into them, and a set of
normalized Container and Mount types so callers do not deal in engine-specific
shapes. A single request-and-mapping core is shared by a Docker adapter and a
Podman adapter that differ only in their default socket path and how they read
compose identity off a container's labels.

This module is a leaf: its only non-stdlib dependency is the Docker SDK. It was
extracted from ballast so a second consumer can share the same abstraction.

## Optional capabilities

The base `Runtime` interface is the stable contract every adapter satisfies:
list, inspect, watch, exec, stop, start, kill, restart, and close. Capabilities
added after v0.2.0 land as separate optional interfaces rather than new
`Runtime` methods, so a new capability never breaks an existing consumer's mock
or alternate `Runtime` implementation. A consumer type-asserts for the
capability it wants:

    if ni, ok := rt.(runtime.NetworkInspector); ok {
        nets, err := ni.ListNetworks(ctx)
        // ...
    }

- `NetworkInspector` (v0.3.0): `ListNetworks(ctx)` returns each network's subnet
  CIDRs, driver, internal flag, and labels, for classifying a destination as
  own-network, LAN, or internet. Both the Docker and Podman adapters satisfy it.
- `Provisioner` (v0.4.0): stand up and tear down throwaway objects to prove a
  restore in a sandbox with no path to production. `PullImage`, `CreateNetwork`
  (always internal, no external routing), `RemoveNetwork`, `CreateVolume`,
  `RemoveVolume`, `CreateContainer` (volume mounts, on a network, no published
  ports, created but started only on request), and `RemoveContainer` (with its
  anonymous volumes). Every create spec carries a `Labels` map so a caller can
  find and sweep orphans after a crash. The caller owns teardown; core does
  not. Both the Docker and Podman adapters satisfy it. This is the surface
  ballast's `ballast verify` command drives.

## Normalized types

- `Container`: id, name, state, labels, mounts, compose project and service,
  image, log driver, env, health, and `Networks`.
- `ContainerNetwork`: one network a container is attached to, and the IP
  addresses it holds on that network.
- `Network`: a network's name, id, driver, internal flag, subnet CIDRs, and
  labels.
- `Mount`: a normalized bind, tmpfs, or named-volume mount.

## Versions

- v0.4.0: provisioning. The `Provisioner` optional capability
  (`PullImage`, `CreateNetwork`, `RemoveNetwork`, `CreateVolume`,
  `RemoveVolume`, `CreateContainer`, `RemoveContainer`) with its `NetworkSpec`,
  `VolumeSpec`, `ContainerSpec`, and `VolumeMount` spec types, for standing up
  a throwaway restore sandbox on an isolated network. Adds an optional
  `ExecSpec.Stdin` field so `Exec` can pipe a dump into a restoring process
  (nil keeps the prior behavior). Both changes are additive: the `Runtime`
  interface is unchanged.
- v0.3.0: network introspection. `NetworkInspector.ListNetworks`, `Network`,
  `ContainerNetwork`, and `Container.Networks`, for egress classification.
- v0.2.0: `Kill` and `Restart`, and `Container` gains image, log driver, env,
  and health.
- v0.1.0: initial extraction from ballast. The `Runtime` abstraction over Docker
  and Podman with normalized `Container` and `Mount` types.

Licensed under GPL-3.0-or-later. See LICENSE.
