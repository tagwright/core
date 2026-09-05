// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtime

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Provisioner is an optional capability a Runtime implementation may satisfy
// in addition to Runtime, for standing up and tearing down throwaway objects:
// a fresh isolated network, empty volumes, and a container created from an
// image with those volumes mounted. It is the surface a consumer's verify
// flow drives to prove a restore in a sandbox that has no path to
// production.
//
// Like NetworkInspector, it is kept as a separate interface rather than a set
// of new methods on Runtime, so adding it never breaks an existing consumer's
// mock or alternate Runtime implementation. A consumer that wants to provision
// type-asserts the value it got back from a constructor (or from Runtime) to
// Provisioner; a consumer that does not care is unaffected.
//
// DockerRuntime and PodmanRuntime both satisfy Provisioner, implemented once
// on the shared engineClient (both embed it) against the Docker Engine
// API-compatible surface Podman also exposes.
//
// Lifecycle ownership. Provisioner does NOT own cleanup: it hands back the id
// or name of each object it creates and the caller is responsible for tearing
// them down, in reverse order, with RemoveContainer, RemoveVolume, and
// RemoveNetwork (a container must go before the volumes and network it holds).
// To make orphan cleanup possible after a crash, every create spec carries a
// Labels map: a caller should stamp its own label on every object so a later
// sweep can find and remove anything a killed run left behind. Starting a
// created container uses the base Runtime.Start; a restore that pipes a dump
// into a container's stdin uses the base Runtime.Exec with ExecSpec.Stdin set.
type Provisioner interface {
	// PullImage pulls ref if the runtime does not already have it locally. It
	// is a no-op when the image is already present, so a caller may call it
	// unconditionally before CreateContainer.
	PullImage(ctx context.Context, ref string) error

	// CreateNetwork creates an isolated network and returns its id. The
	// network is always internal (no external routing, no gateway to the host
	// or the internet), so a container attached to it cannot reach production
	// services or the network at large. That isolation is the entire point of
	// the capability and is not configurable: a restored copy proven on such a
	// network is a DORA-style segregation fact the caller can assert directly
	// via NetworkInspector.ListNetworks (the created network reports
	// Internal == true).
	CreateNetwork(ctx context.Context, spec NetworkSpec) (id string, err error)

	// RemoveNetwork removes a network by id or name. It fails if a container
	// is still attached, so remove containers first.
	RemoveNetwork(ctx context.Context, id string) error

	// CreateVolume creates a fresh empty named volume and returns its name. A
	// verify restores into one of these, never a real service volume.
	CreateVolume(ctx context.Context, spec VolumeSpec) (name string, err error)

	// RemoveVolume removes a named volume. It fails if a container still holds
	// it, so remove the container first (or use RemoveContainer, which also
	// drops the container's anonymous volumes).
	RemoveVolume(ctx context.Context, name string) error

	// CreateContainer creates a container from spec.Image with the spec's
	// volume mounts, on the spec's network, with its env, labels, and optional
	// command or entrypoint override. It publishes no ports. The container is
	// created but not started unless spec.Start is set (starting a
	// separately-created container otherwise uses the base Runtime.Start). It
	// returns the container id even on a start error, so the caller can still
	// tear the container down.
	CreateContainer(ctx context.Context, spec ContainerSpec) (id string, err error)

	// RemoveContainer removes a container by id or name along with its
	// anonymous volumes. force removes a running container (equivalent to a
	// stop-then-remove) rather than failing.
	RemoveContainer(ctx context.Context, id string, force bool) error
}

// NetworkSpec describes a throwaway network to create. The network is always
// created internal (see Provisioner.CreateNetwork), so the spec carries only
// the name and the labels a caller stamps on for later cleanup.
type NetworkSpec struct {
	Name   string
	Labels map[string]string
}

// VolumeSpec describes a fresh empty named volume to create.
type VolumeSpec struct {
	Name   string
	Labels map[string]string
}

// VolumeMount attaches a named volume into a container at a path. It is the
// create-side counterpart to the read-side Mount type: verify mounts the fresh
// volume it restored into at the path the real service expects.
type VolumeMount struct {
	// Volume is the named volume to mount, as returned by CreateVolume.
	Volume string
	// Destination is the container-side path to mount it at.
	Destination string
	// ReadOnly mounts the volume read-only when true.
	ReadOnly bool
}

// ContainerSpec describes a throwaway container to create. It is deliberately
// the minimal surface verify needs: an image, volume mounts, a network, env,
// labels, an optional command/entrypoint override, and whether to start it. It
// publishes no ports, so a created container is never reachable from outside
// the (internal) network it is placed on.
type ContainerSpec struct {
	// Name is the container name. Empty lets the engine assign one.
	Name string
	// Image is the image reference to create from. Call PullImage first if it
	// may not be present locally.
	Image string
	// Cmd overrides the image's default command when non-empty.
	Cmd []string
	// Entrypoint overrides the image's default entrypoint when non-empty.
	Entrypoint []string
	// Env holds environment entries as KEY=VALUE strings.
	Env []string
	// Labels are stamped on the container for identification and cleanup.
	Labels map[string]string
	// Mounts attaches named volumes into the container.
	Mounts []VolumeMount
	// Network is the network name or id to attach to. Empty uses the engine's
	// default network; a verify passes the isolated network from CreateNetwork.
	Network string
	// Start starts the container immediately after creating it.
	Start bool
}

// PullImage pulls ref if it is not already present locally.
func (e *engineClient) PullImage(ctx context.Context, ref string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	// An inspect that succeeds means the image is already local, so the pull
	// (which reaches out to a registry) is skipped. Only a genuine not-found
	// falls through to the pull; any other inspect error is real and returned.
	if _, inspectErr := cli.ImageInspect(ctx, ref); inspectErr == nil {
		return nil
	} else if !client.IsErrNotFound(inspectErr) {
		return fmt.Errorf("runtime/%s: inspect image %s: %w", e.engine, ref, inspectErr)
	}

	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("runtime/%s: pull image %s: %w", e.engine, ref, err)
	}
	defer rc.Close()

	// The pull only completes once its progress stream is drained to EOF, so
	// the body is read to completion and discarded rather than parsed.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("runtime/%s: pull image %s: %w", e.engine, ref, err)
	}
	return nil
}

// CreateNetwork creates an internal (isolated) network and returns its id.
func (e *engineClient) CreateNetwork(ctx context.Context, spec NetworkSpec) (string, error) {
	cli, err := e.clientFor()
	if err != nil {
		return "", err
	}

	resp, err := cli.NetworkCreate(ctx, spec.Name, network.CreateOptions{
		Driver:   "bridge",
		Internal: true,
		Labels:   spec.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("runtime/%s: create network %s: %w", e.engine, spec.Name, err)
	}
	return resp.ID, nil
}

// RemoveNetwork removes a network by id or name.
func (e *engineClient) RemoveNetwork(ctx context.Context, id string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if err := cli.NetworkRemove(ctx, id); err != nil {
		return fmt.Errorf("runtime/%s: remove network %s: %w", e.engine, id, err)
	}
	return nil
}

// CreateVolume creates a fresh empty named volume and returns its name.
func (e *engineClient) CreateVolume(ctx context.Context, spec VolumeSpec) (string, error) {
	cli, err := e.clientFor()
	if err != nil {
		return "", err
	}

	vol, err := cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   spec.Name,
		Labels: spec.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("runtime/%s: create volume %s: %w", e.engine, spec.Name, err)
	}
	return vol.Name, nil
}

// RemoveVolume removes a named volume. It does not force: a volume still held
// by a container is removed only after the container is gone.
func (e *engineClient) RemoveVolume(ctx context.Context, name string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if err := cli.VolumeRemove(ctx, name, false); err != nil {
		return fmt.Errorf("runtime/%s: remove volume %s: %w", e.engine, name, err)
	}
	return nil
}

// CreateContainer creates a container from spec and, if spec.Start, starts it.
func (e *engineClient) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	cli, err := e.clientFor()
	if err != nil {
		return "", err
	}

	cfg := &container.Config{
		Image:  spec.Image,
		Env:    spec.Env,
		Labels: spec.Labels,
	}
	// Cmd and Entrypoint override the image defaults only when set: an empty
	// slice would blank the image's own values, which is not the intent.
	if len(spec.Cmd) > 0 {
		cfg.Cmd = spec.Cmd
	}
	if len(spec.Entrypoint) > 0 {
		cfg.Entrypoint = spec.Entrypoint
	}

	hostCfg := &container.HostConfig{}
	if spec.Network != "" {
		hostCfg.NetworkMode = container.NetworkMode(spec.Network)
	}
	for _, m := range spec.Mounts {
		hostCfg.Mounts = append(hostCfg.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.Volume,
			Target:   m.Destination,
			ReadOnly: m.ReadOnly,
		})
	}

	// No PortBindings are ever set, so the container publishes nothing to the
	// host: a throwaway restore is never reachable from outside its network.
	created, err := cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("runtime/%s: create container %s: %w", e.engine, spec.Name, err)
	}

	if spec.Start {
		if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			// The id is returned alongside the error so the caller can still
			// tear the created-but-unstarted container down.
			return created.ID, fmt.Errorf("runtime/%s: start container %s: %w", e.engine, created.ID, err)
		}
	}
	return created.ID, nil
}

// RemoveContainer removes a container by id or name along with its anonymous
// volumes. force removes a running container instead of failing.
func (e *engineClient) RemoveContainer(ctx context.Context, id string, force bool) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{
		Force:         force,
		RemoveVolumes: true,
	}); err != nil {
		return fmt.Errorf("runtime/%s: remove container %s: %w", e.engine, id, err)
	}
	return nil
}
