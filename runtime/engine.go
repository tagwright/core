// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// composeIdentityFunc resolves a container's compose project and service
// names from its labels. Docker and Podman disagree on which label keys
// carry that grouping (Podman's own tooling adds an io.podman.compose.*
// pair alongside, or instead of, Docker's com.docker.compose.*), so each
// engineClient is handed the mapping its runtime needs rather than the
// shared machinery hard-coding one label set.
type composeIdentityFunc func(labels map[string]string) (project, service string)

// engineClient is the request and mapping machinery shared by every adapter
// that talks to a Docker Engine API-compatible socket. DockerRuntime and
// PodmanRuntime both embed one; the only things that differ between the two
// engines are the socket path and how compose project/service identity is
// read off a container's labels, both supplied at construction time.
//
// The client is created lazily on first use and cached, so constructing an
// engineClient never touches the socket: nothing fails until a method that
// actually needs the daemon is called.
type engineClient struct {
	// engine names the runtime for error messages, e.g. "docker" or
	// "podman".
	engine string

	// socket is the path to the API socket, e.g. /var/run/docker.sock or
	// /run/podman/podman.sock.
	socket string

	// identity resolves compose project/service names from a container's
	// labels.
	identity composeIdentityFunc

	mu     sync.Mutex
	client *client.Client
}

// clientFor returns the cached engine API client, creating it on first call.
// API version negotiation means the client adapts to whatever the daemon on
// the other end of the socket speaks, rather than pinning a version core
// has to keep in lockstep with the engine.
func (e *engineClient) clientFor() (*client.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil {
		return e.client, nil
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+e.socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: new client: %w", e.engine, err)
	}
	e.client = cli
	return cli, nil
}

// List returns every container the runtime knows about, running or not.
func (e *engineClient) List(ctx context.Context) ([]Container, error) {
	cli, err := e.clientFor()
	if err != nil {
		return nil, err
	}

	listed, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: list containers: %w", e.engine, err)
	}

	summaries := listed.Items
	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}

		mounts := make([]Mount, 0, len(s.Mounts))
		for _, m := range s.Mounts {
			mounts = append(mounts, mapMountPoint(m))
		}

		var networks []ContainerNetwork
		if s.NetworkSettings != nil {
			networks = mapContainerNetworks(s.NetworkSettings.Networks)
		}

		project, service := e.identity(s.Labels)
		out = append(out, Container{
			ID:       s.ID,
			Name:     name,
			State:    string(s.State),
			Labels:   s.Labels,
			Mounts:   mounts,
			Project:  project,
			Service:  service,
			Image:    s.Image,
			Networks: networks,
		})
	}
	return out, nil
}

// Inspect returns a single container by ID or name.
func (e *engineClient) Inspect(ctx context.Context, id string) (Container, error) {
	cli, err := e.clientFor()
	if err != nil {
		return Container{}, err
	}

	inspected, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Container{}, fmt.Errorf("runtime/%s: inspect container %s: %w", e.engine, id, err)
	}
	info := inspected.Container

	var labels map[string]string
	image := ""
	var env []string
	if info.Config != nil {
		labels = info.Config.Labels
		image = info.Config.Image
		env = info.Config.Env
	}

	state := ""
	health := ""
	exitCode := 0
	oomKilled := false
	if info.State != nil {
		// State.Status and Health.Status are the strongly-typed ContainerState
		// and HealthStatus enums in the moby api types, so each is converted
		// back to the plain string core's Container promises.
		state = string(info.State.Status)
		exitCode = info.State.ExitCode
		oomKilled = info.State.OOMKilled
		// State.Health is nil when the container has no HEALTHCHECK, so the
		// pointer must be guarded before reading its status.
		if info.State.Health != nil {
			health = string(info.State.Health.Status)
		}
	}

	// HostConfig is present on inspect but absent from the list summary, so
	// LogDriver and RestartPolicy only populate here. They stay empty when
	// HostConfig is nil.
	logDriver := ""
	if info.HostConfig != nil {
		logDriver = info.HostConfig.LogConfig.Type
	}
	restartPolicy := restartPolicyName(info.HostConfig)

	mounts := make([]Mount, 0, len(info.Mounts))
	for _, m := range info.Mounts {
		mounts = append(mounts, mapMountPoint(m))
	}

	var networks []ContainerNetwork
	var ports []Port
	if info.NetworkSettings != nil {
		networks = mapContainerNetworks(info.NetworkSettings.Networks)
		ports = mapContainerPorts(info.NetworkSettings.Ports)
	}

	project, service := e.identity(labels)
	return Container{
		ID:            info.ID,
		Name:          strings.TrimPrefix(info.Name, "/"),
		State:         state,
		Labels:        labels,
		Mounts:        mounts,
		Project:       project,
		Service:       service,
		Image:         image,
		LogDriver:     logDriver,
		Env:           env,
		Health:        health,
		Networks:      networks,
		Ports:         ports,
		RestartPolicy: restartPolicy,
		ExitCode:      exitCode,
		OOMKilled:     oomKilled,
		// RestartCount lives on the inspect base, not State, and is always
		// present, so it needs no nil guard.
		RestartCount: info.RestartCount,
	}, nil
}

// restartPolicyName reads the configured restart-policy name off an inspect
// HostConfig, guarding the nil HostConfig the engine reports for a container
// that carries none (or that a partial response omits) so the read never
// panics. The value is the policy the container was created with, e.g. "no",
// "always", "unless-stopped", or "on-failure", and empty when HostConfig is
// nil. Unlike the RestartCount live counter, this is a static configuration
// field, so no separate list-vs-inspect distinction applies beyond HostConfig
// being inspect-only.
func restartPolicyName(hc *container.HostConfig) string {
	if hc == nil {
		return ""
	}
	return string(hc.RestartPolicy.Name)
}

// mapContainerPorts translates the Docker Engine API's published-port map
// (NetworkSettings.Ports, keyed by "port/proto" with a list of host bindings
// per key) into core's normalized Port slice. It emits one Port per host
// binding, so a port published to both an IPv4 and an IPv6 host address yields
// two entries; a key with no bindings (a port exposed by the image but not
// published to the host) yields none. Podman's compat API reports ports in the
// same shape, so this mapping is engine-agnostic.
//
// The container port comes from the key (network.Port -> 80, "tcp"); the host
// port comes from the binding string ("8080" -> 8080). A host-port string that
// does not parse is carried as 0 rather than failing the whole mapping, which
// does not happen for a well-formed engine response. The result is sorted for a
// deterministic order (by container port, then protocol, host IP, host port),
// since the engine's map iteration is not, and a churning order would break a
// consumer's regenerate-and-compare invariant.
//
// The moby api types make the binding's HostIP a netip.Addr, so an unbound
// binding carries the zero Addr. That is stringified only when valid: a zero
// (unbound) host address maps to "", not the literal "invalid IP" that
// netip.Addr{}.String() returns. runtime.go promises "" here, and a consumer
// classifying off-host reachability keys on HostIP, so a mis-mapped unbound
// binding would otherwise read as reachable.
func mapContainerPorts(ports network.PortMap) []Port {
	if len(ports) == 0 {
		return nil
	}

	out := make([]Port, 0, len(ports))
	for key, bindings := range ports {
		containerPort := int(key.Num())
		proto := string(key.Proto())
		for _, b := range bindings {
			hostPort := 0
			if b.HostPort != "" {
				if n, err := strconv.Atoi(b.HostPort); err == nil {
					hostPort = n
				}
			}
			hostIP := ""
			if b.HostIP.IsValid() {
				hostIP = b.HostIP.String()
			}
			out = append(out, Port{
				ContainerPort: containerPort,
				HostPort:      hostPort,
				Protocol:      proto,
				HostIP:        hostIP,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ContainerPort != out[j].ContainerPort {
			return out[i].ContainerPort < out[j].ContainerPort
		}
		if out[i].Protocol != out[j].Protocol {
			return out[i].Protocol < out[j].Protocol
		}
		if out[i].HostIP != out[j].HostIP {
			return out[i].HostIP < out[j].HostIP
		}
		return out[i].HostPort < out[j].HostPort
	})
	return out
}

// mapContainerNetworks translates the Docker Engine API's per-network
// endpoint settings (keyed by network name, shared verbatim between the
// list summary's NetworkSettingsSummary and inspect's NetworkSettings) into
// core's normalized ContainerNetwork slice. In the moby api types the endpoint
// IP fields are already netip.Addr, so a family a container holds no address on
// (most commonly IPv6) decodes to the zero Addr; those are skipped via
// IsValid() rather than parsed, so an unset address contributes nothing rather
// than failing the whole mapping. The result is sorted by network name for a
// deterministic order, since map iteration is not.
func mapContainerNetworks(nets map[string]*network.EndpointSettings) []ContainerNetwork {
	if len(nets) == 0 {
		return nil
	}

	out := make([]ContainerNetwork, 0, len(nets))
	for name, ep := range nets {
		if ep == nil {
			continue
		}

		cn := ContainerNetwork{Name: name, ID: ep.NetworkID}
		if ep.IPAddress.IsValid() {
			cn.IPs = append(cn.IPs, ep.IPAddress)
		}
		if ep.GlobalIPv6Address.IsValid() {
			cn.IPs = append(cn.IPs, ep.GlobalIPv6Address)
		}
		out = append(out, cn)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ListNetworks returns every network the runtime knows about, with its
// subnet CIDRs and internal flag. It satisfies NetworkInspector for both
// DockerRuntime and PodmanRuntime, which embed engineClient.
func (e *engineClient) ListNetworks(ctx context.Context) ([]Network, error) {
	cli, err := e.clientFor()
	if err != nil {
		return nil, err
	}

	listed, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: list networks: %w", e.engine, err)
	}

	summaries := listed.Items
	out := make([]Network, 0, len(summaries))
	for _, n := range summaries {
		out = append(out, mapNetworkSummary(n))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// mapNetworkSummary translates a Docker Engine API network summary into
// core's normalized Network type. network.Summary embeds network.Network in
// the moby api types, so NetworkList already returns IPAM, Driver, Internal,
// and Labels in full: no follow-up NetworkInspect call per network is needed
// to populate them. The IPAM subnet is already a netip.Prefix, so an empty or
// absent subnet decodes to the zero Prefix and is skipped via IsValid(). A
// malformed subnet, by contrast, now fails JSON decoding of the whole
// NetworkList response inside the client before this mapping runs, so the old
// per-entry skip of an unparseable subnet is no longer reachable here (see the
// finding 1b note in runtime.go).
func mapNetworkSummary(n network.Summary) Network {
	subnets := make([]netip.Prefix, 0, len(n.IPAM.Config))
	for _, c := range n.IPAM.Config {
		if !c.Subnet.IsValid() {
			continue
		}
		subnets = append(subnets, c.Subnet)
	}

	return Network{
		Name:     n.Name,
		ID:       n.ID,
		Driver:   n.Driver,
		Internal: n.Internal,
		Subnets:  subnets,
		Labels:   n.Labels,
	}
}

// mapMountPoint translates a Docker Engine API mount point into core's
// normalized Mount type. Anything the engine reports that is not a
// recognized bind or tmpfs mount is treated as a named volume, which is the
// common case and the one a consumer most needs to get right (it is what gets
// dumped or archived). Podman's compat API reports mounts in the same
// shape, so this mapping is engine-agnostic.
func mapMountPoint(m container.MountPoint) Mount {
	mt := MountVolume
	switch m.Type {
	case mount.TypeBind:
		mt = MountBind
	case mount.TypeTmpfs:
		mt = MountTmpfs
	case mount.TypeVolume:
		mt = MountVolume
	}

	return Mount{
		Type:        mt,
		Name:        m.Name,
		Source:      m.Source,
		Destination: m.Destination,
		ReadOnly:    !m.RW,
	}
}

// mapEventAction translates a Docker Engine API event action into core's
// normalized EventType. Beyond the four lifecycle transitions (start, stop,
// die, destroy) it also surfaces the OOM kill and the two health-status
// transitions (healthy, unhealthy) a watch-path consumer keys on. Actions a
// consumer does not act on (exec, resize, the bare "health_status" prefix
// without a suffix, and the like) are reported as not-ok so the caller can
// skip them.
//
// Podman's compat event stream reuses most of the same action vocabulary,
// but not all of it: where a real Docker daemon emits "destroy" for a
// container's removal, Podman's compat API (confirmed against a live
// Podman 5.8 socket, not merely its documentation) emits "remove" instead,
// and never emits "destroy" for a container at all. Both are mapped to
// EventDestroy here so daemon/watch.go's die-or-destroy unregistration
// fires correctly on both runtimes; a real Docker daemon has never been
// observed to emit ActionRemove for a container (only for other resource
// types), so widening the match costs Docker nothing.
func mapEventAction(action events.Action) (EventType, bool) {
	switch action {
	case events.ActionStart:
		return EventStart, true
	case events.ActionStop:
		return EventStop, true
	case events.ActionDie:
		return EventDie, true
	case events.ActionDestroy, events.ActionRemove:
		return EventDestroy, true
	case events.ActionOOM:
		return EventOOM, true
	case events.ActionHealthStatusHealthy:
		return EventHealthStatusHealthy, true
	case events.ActionHealthStatusUnhealthy:
		return EventHealthStatusUnhealthy, true
	default:
		return "", false
	}
}

// Watch streams lifecycle events until ctx is cancelled. The error channel
// carries a terminal error and is then closed alongside the event channel.
func (e *engineClient) Watch(ctx context.Context) (<-chan Event, <-chan error) {
	out := make(chan Event)
	errs := make(chan error, 1)

	cli, err := e.clientFor()
	if err != nil {
		errs <- err
		close(out)
		close(errs)
		return out, errs
	}

	// The moby client reshapes Events to return a single EventsResult carrying
	// the message and error channels, and replaces the old filters.Args builder
	// with client.Filters. events.ContainerEventType is a typed enum, so it is
	// converted to a plain string for the filter value.
	stream := cli.Events(ctx, client.EventsListOptions{
		Filters: make(client.Filters).Add("type", string(events.ContainerEventType)),
	})
	msgs, errCh := stream.Messages, stream.Err

	go func() {
		defer close(out)
		defer close(errs)

		for {
			select {
			case <-ctx.Done():
				return

			case err, ok := <-errCh:
				if !ok {
					return
				}
				if err != nil {
					errs <- err
				}
				return

			case msg, ok := <-msgs:
				if !ok {
					return
				}
				et, ok := mapEventAction(msg.Action)
				if !ok {
					continue
				}
				out <- Event{
					Type:   et,
					ID:     msg.Actor.ID,
					Name:   msg.Actor.Attributes["name"],
					Labels: msg.Actor.Attributes,
				}
			}
		}
	}()

	return out, errs
}

// Exec runs a command inside a running container and returns a handle whose
// Stdout streams the command's standard output as it is produced, which
// matters because the caller pipes a live dump into restic --stdin rather
// than buffering it. Standard error is captured separately and folded into
// the error Wait returns on a non-zero exit.
//
// When spec.Stdin is non-nil (a stream-restore piping a dump into the
// restoring process) it is attached to the command's standard input and
// copied in a goroutine, then the write half is closed so the command sees
// EOF. When spec.Stdin is nil this is a no-op and Exec behaves exactly as it
// always has.
func (e *engineClient) Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error) {
	cli, err := e.clientFor()
	if err != nil {
		return nil, err
	}

	created, err := cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          spec.Cmd,
		User:         spec.User,
		AttachStdin:  spec.Stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: exec create on %s: %w", e.engine, id, err)
	}

	attach, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: exec attach on %s: %w", e.engine, id, err)
	}

	// Feed stdin to the command when the caller supplied it. The copy runs in
	// its own goroutine so a large dump streams in rather than being buffered,
	// and the write half is closed on completion so the command reads EOF and
	// exits. A copy error (most often the command closing its input early) is
	// left to surface through the command's own exit code in Wait.
	if spec.Stdin != nil {
		go func() {
			_, _ = io.Copy(attach.Conn, spec.Stdin)
			_ = attach.CloseWrite()
		}()
	}

	stdoutR, stdoutW := io.Pipe()
	var stderrBuf bytes.Buffer
	done := make(chan struct{})

	// The engine multiplexes stdout and stderr onto one stream when the exec
	// was not created with a TTY. stdcopy demultiplexes it as it arrives, so
	// stdout keeps flowing to the pipe reader instead of waiting for the
	// command to finish.
	go func() {
		defer close(done)
		defer attach.Close()
		_, copyErr := stdcopy.StdCopy(stdoutW, &stderrBuf, attach.Reader)
		stdoutW.CloseWithError(copyErr)
	}()

	cmd := spec.Cmd
	wait := func() (int, error) {
		<-done

		inspect, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return 0, fmt.Errorf("runtime/%s: exec inspect on %s: %w", e.engine, id, err)
		}

		if inspect.ExitCode != 0 {
			return inspect.ExitCode, fmt.Errorf("runtime/%s: exec %v on %s exited %d: %s",
				e.engine, cmd, id, inspect.ExitCode, strings.TrimSpace(stderrBuf.String()))
		}
		return inspect.ExitCode, nil
	}

	return &ExecHandle{Stdout: stdoutR, Wait: wait}, nil
}

// Stop stops a running container, waiting up to timeoutSeconds before a kill.
func (e *engineClient) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	timeout := timeoutSeconds
	if _, err := cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("runtime/%s: stop %s: %w", e.engine, id, err)
	}
	return nil
}

// Start starts a stopped container.
func (e *engineClient) Start(ctx context.Context, id string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("runtime/%s: start %s: %w", e.engine, id, err)
	}
	return nil
}

// Kill sends a signal to a running container, e.g. "SIGHUP" to prompt a
// collector to reload its configuration.
func (e *engineClient) Kill(ctx context.Context, id string, signal string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if _, err := cli.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: signal}); err != nil {
		return fmt.Errorf("runtime/%s: kill %s: %w", e.engine, id, err)
	}
	return nil
}

// Restart restarts a container, using the runtime's default stop timeout.
func (e *engineClient) Restart(ctx context.Context, id string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if _, err := cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("runtime/%s: restart %s: %w", e.engine, id, err)
	}
	return nil
}

// Close releases the underlying client.
func (e *engineClient) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client == nil {
		return nil
	}
	err := e.client.Close()
	e.client = nil
	return err
}
