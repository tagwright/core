// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package runtime abstracts a container runtime (Docker or Podman) behind the
// small set of operations a consumer needs: discover containers and their mounts,
// watch the socket for lifecycle changes, exec into a container to quiesce or
// dump it, and stop or start it for a cold backup.
//
// The interface is deliberately kept free of any tool-specific type. It was
// shaped against exactly one real consumer before being lifted into
// github.com/tagwright/core, which is the discipline that keeps the abstraction
// honest.
//
// The Docker adapter lands first. The Podman adapter follows behind the
// same interface, talking to Podman's Docker-compatible compat API and
// absorbing the socket-path and compose-label differences.
package runtime

import (
	"context"
	"errors"
	"io"
	"net/netip"
)

// ErrNotImplemented is returned by adapter methods that are not wired up yet.
var ErrNotImplemented = errors.New("runtime: not implemented")

// Runtime is a container runtime a consumer can drive. Implementations must be safe
// for concurrent use by multiple goroutines.
type Runtime interface {
	// List returns every container the runtime knows about, running or not.
	List(ctx context.Context) ([]Container, error)

	// Inspect returns a single container by ID or name.
	Inspect(ctx context.Context, id string) (Container, error)

	// Watch streams lifecycle events until ctx is cancelled. The error channel
	// carries a terminal error and is then closed alongside the event channel.
	Watch(ctx context.Context) (<-chan Event, <-chan error)

	// Exec runs a command inside a running container and returns a handle whose
	// Stdout the caller reads (for stream backups it is piped straight into the
	// engine's stdin) before calling Wait for the exit code.
	Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error)

	// Stop stops a running container, waiting up to timeoutSeconds before a kill.
	Stop(ctx context.Context, id string, timeoutSeconds int) error

	// Start starts a stopped container.
	Start(ctx context.Context, id string) error

	// Kill sends a signal to a running container, e.g. "SIGHUP" to prompt a
	// collector to reload its configuration.
	Kill(ctx context.Context, id string, signal string) error

	// Restart restarts a container, using the runtime's default stop timeout.
	Restart(ctx context.Context, id string) error

	// Close releases the underlying client.
	Close() error
}

// NetworkInspector is an optional capability a Runtime implementation may
// satisfy in addition to Runtime. It is kept as a separate interface,
// rather than a new method on Runtime, so that adding it never breaks an
// existing consumer's mock or alternate implementation of Runtime: a
// consumer that wants network introspection type-asserts the value it got
// back from a constructor (or from Runtime) to NetworkInspector, and a
// consumer that does not care about networks is unaffected.
//
// DockerRuntime and PodmanRuntime both satisfy NetworkInspector.
type NetworkInspector interface {
	// ListNetworks returns every network the runtime knows about, with its
	// subnet CIDRs and whether it is marked internal.
	ListNetworks(ctx context.Context) ([]Network, error)
}

// Network is the normalized view of a container network across runtimes.
type Network struct {
	Name     string
	ID       string
	Driver   string
	Internal bool

	// Subnets are the network's IPAM subnet CIDRs. In the moby api types the
	// IPAM subnet is a netip.Prefix, so an empty or absent subnet decodes to the
	// zero Prefix and is skipped, while a malformed subnet now fails JSON
	// decoding of the whole NetworkList response inside the client, before core's
	// mapping runs (finding 1b): a single malformed subnet from the daemon fails
	// the whole ListNetworks call rather than dropping one entry. A well-formed
	// daemon response never triggers this.
	Subnets []netip.Prefix

	Labels map[string]string
}

// ContainerNetwork is one network a container is attached to, along with
// the IP addresses it holds on that network.
type ContainerNetwork struct {
	Name string
	ID   string
	IPs  []netip.Addr
}

// Port is one published host binding of a container port, as the runtime's
// inspect reports it.
//
// One Port is emitted per host binding, mirroring what inspect's
// NetworkSettings.Ports carries: a container port published to two host
// interfaces (a v4 0.0.0.0 binding and a v6 :: binding, say) yields two
// entries, and a container port that is exposed but not published to the host
// carries no binding and so contributes no Port at all. A consumer deciding
// whether a service is reachable from off-host reads HostPort together with
// HostIP: an empty or 0.0.0.0 HostIP means "every interface", a loopback
// HostIP (127.0.0.1 or ::1) means host-only.
type Port struct {
	// ContainerPort is the container-side port number, e.g. 80, read from the
	// inspect key. In the moby api types that key is a strongly-typed
	// network.Port, so a malformed key no longer yields 0 here: it fails JSON
	// decoding of the whole ContainerInspect response inside the client, before
	// core's mapping runs (the finding 1b whole-response-fail behavior, shared
	// with Network.Subnets). A well-formed engine response is therefore the only
	// case this mapping sees.
	ContainerPort int

	// HostPort is the host-side published port number the binding maps to. It
	// is 0 only when the binding carries no host port, which in practice does
	// not occur (an exposed-but-unpublished port has no binding and so yields
	// no Port at all).
	HostPort int

	// Protocol is the port's protocol, "tcp", "udp", or "sctp", read off the
	// inspect key's suffix.
	Protocol string

	// HostIP is the host interface the port is bound on, as inspect provides
	// it: an explicit address, "0.0.0.0"/"::" for all interfaces, or "" when the
	// binding is unbound. In the moby api types the binding's host address is a
	// netip.Addr, so an unbound binding carries the zero Addr, which core maps to
	// "" (not the literal "invalid IP" that netip.Addr{}.String() returns; this
	// is the finding 1a contract a reachability consumer relies on).
	HostIP string
}

// Container is the normalized view of a container across runtimes.
type Container struct {
	ID      string
	Name    string
	State   string // running, exited, paused, ...
	Labels  map[string]string
	Mounts  []Mount
	Project string // com.docker.compose.project, empty if not a compose service
	Service string // com.docker.compose.service, empty if not a compose service

	// Image is the container's image reference. Populated on both List and
	// Inspect.
	Image string

	// LogDriver is the effective logging driver, e.g. "json-file", "local",
	// "journald". Inspect-only (the list summary carries no HostConfig), and
	// empty when unknown.
	LogDriver string

	// Env holds the container's environment entries as KEY=VALUE strings.
	// Inspect-only. Core surfaces the raw slice: callers that only need the
	// names must split it themselves and must not log the values.
	Env []string

	// Health is the container health status when a HEALTHCHECK is defined,
	// e.g. "healthy", "unhealthy", "starting". Empty when the container has no
	// healthcheck. Inspect-only.
	Health string

	// ExitCode is the exit code of the container's main process from its last
	// run, read off Docker's inspect State.ExitCode. It is 0 for a container
	// that is still running or that exited cleanly, so a consumer treating a
	// non-zero value as a failure must pair it with the State ("exited") or a
	// die event rather than reading it in isolation. Inspect-only.
	ExitCode int

	// OOMKilled reports whether the container's last exit was the kernel OOM
	// killer reaping it, read off Docker's inspect State.OOMKilled. Inspect-only.
	OOMKilled bool

	// RestartCount is how many times the runtime has restarted this container
	// under its restart policy, read off Docker's inspect RestartCount. A
	// consumer watching for a crash loop reads this alongside the start events
	// from Watch; core surfaces the count and leaves the loop policy to the
	// consumer. Inspect-only.
	RestartCount int

	// Networks lists the container's network attachments and the IP
	// addresses it holds on each. Unlike Image/LogDriver/Env/Health, the
	// list summary carries this data at no extra cost (it is already part
	// of the same API response), so Networks is populated on both List and
	// Inspect.
	Networks []ContainerNetwork

	// Ports lists the container's published host port bindings, one entry per
	// binding, read off Docker's inspect NetworkSettings.Ports. HostConfig is
	// not needed for these (the bindings live on NetworkSettings), but the
	// list summary's port view is thinner, so core populates Ports on Inspect
	// only and leaves it nil on List. It is nil when the container publishes
	// nothing. See Port for the per-binding shape and the reachability
	// interpretation of HostIP.
	Ports []Port

	// RestartPolicy is the container's configured restart policy name, e.g.
	// "no", "always", "unless-stopped", "on-failure", read off Docker's
	// inspect HostConfig.RestartPolicy.Name. Unlike RestartCount (a live
	// counter), this is the policy the container was created with. It is
	// Inspect-only (the list summary carries no HostConfig) and empty when
	// unknown. A consumer distinguishes a long-running service from a one-shot
	// job by reading this rather than inferring it from State.
	RestartPolicy string
}

// MountType distinguishes the kinds of mount a consumer cares about.
type MountType string

const (
	MountVolume MountType = "volume"
	MountBind   MountType = "bind"
	MountTmpfs  MountType = "tmpfs"
)

// Mount is one filesystem mount attached to a container.
type Mount struct {
	Type        MountType
	Name        string // named-volume name, empty for binds and tmpfs
	Source      string // host-side path, empty for tmpfs
	Destination string // container-side path
	ReadOnly    bool
}

// EventType is a container lifecycle transition.
type EventType string

const (
	EventStart   EventType = "start"
	EventStop    EventType = "stop"
	EventDie     EventType = "die"
	EventDestroy EventType = "destroy"

	// EventOOM is the kernel OOM killer reaping a container's main process,
	// from Docker's "oom" event action. It arrives on its own, ahead of the
	// "die" the reaped process then triggers, so a consumer that wants to
	// distinguish an out-of-memory kill from an ordinary non-zero exit keys on
	// this rather than inferring it from the die alone.
	EventOOM EventType = "oom"

	// EventHealthStatusHealthy and EventHealthStatusUnhealthy are a container's
	// healthcheck transitioning, from Docker's "health_status: healthy" and
	// "health_status: unhealthy" event actions. Docker emits one only when the
	// aggregated health state changes, not on every probe, so each event is an
	// edge a consumer can act on directly. The bare "health_status" action
	// (with no healthy/unhealthy suffix) is not one of these and is not
	// surfaced.
	EventHealthStatusHealthy   EventType = "health_status: healthy"
	EventHealthStatusUnhealthy EventType = "health_status: unhealthy"
)

// Event is a single lifecycle change on the socket.
type Event struct {
	Type   EventType
	ID     string
	Name   string
	Labels map[string]string
}

// ExecSpec describes a command to run inside a container.
type ExecSpec struct {
	Cmd  []string
	User string // empty means the container's default user

	// Stdin, when non-nil, is attached to the command's standard input and
	// copied to completion, after which the write side is closed so the
	// command sees EOF. It is nil for the common case (a quiesce or a
	// dump-producing command that reads nothing on stdin); a stream-restore
	// sets it to pipe a backup dump into the restoring process. When nil, Exec
	// behaves exactly as it always has, so this field is backward compatible
	// with every existing caller.
	Stdin io.Reader
}

// ExecHandle is a running exec. The caller reads Stdout to completion, then
// calls Wait to learn the exit code. Stderr is captured separately for logging.
type ExecHandle struct {
	Stdout io.Reader
	Wait   func() (exitCode int, err error)
}
