// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package runtimetest provides Runtime, the canonical fake core.Runtime for
// the tagwright suite's wiring (Level 2) tests. It replaces the divergent
// hand-rolled fakeRuntime copies each tool grew, and their drift risk, with a
// single fake shaped for the one thing those tests exist to prove: that a
// failure on a runtime operation SURFACES rather than being silently swallowed.
//
// To that end the fake carries three things a happy-path double does not:
//
//   - A fault knob on every operation (Faults). Set Faults.Exec, and the next
//     Exec returns it; set Faults.List, and List does; and so on. A wiring test
//     that never trips a knob is not testing the wiring, only the happy path,
//     which the suite's audit found is where the fake tier misses every real
//     bug. The knob is how a test injects the failure it then asserts surfaces.
//   - A scripted event channel (Emit, Fail, CloseWatch), so a test can drive the
//     watch loop through a start/die/destroy sequence, a mid-stream error, or a
//     clean end of stream, deterministically.
//   - A controllable fake Clock, so the debounce and the scheduler advance on
//     the test's terms rather than the wall clock's.
//
// It implements runtime.Runtime, runtime.Provisioner, and
// runtime.NetworkInspector, so it is a drop-in wherever a tool takes a
// core.Runtime. It is safe for concurrent use: a daemon under test drives it
// from its own goroutines while the test reads its recordings.
package runtimetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tagwright/core/runtime"
)

// Faults holds one injectable error per operation. A non-nil entry makes the
// matching method return that error instead of its normal result, which is how
// a wiring test forces a failure and then asserts it surfaces. The zero Faults
// injects nothing, so a fake left at its defaults behaves as a happy path.
type Faults struct {
	List     error
	Inspect  error
	Exec     error
	Stop     error
	Start    error
	Kill     error
	Restart  error
	Close    error
	Watch    error // delivered on Watch's error channel rather than returned

	// Provisioner knobs.
	PullImage       error
	CreateNetwork   error
	RemoveNetwork   error
	CreateVolume    error
	RemoveVolume    error
	CreateContainer error
	RemoveContainer error

	// NetworkInspector knob.
	ListNetworks error
}

// ExecResult is a canned exec outcome matched to a command by substring, the
// same shape ballast's original hand-rolled fake used: the last match on the
// command's final argument wins, its Stdout is returned to the reader, and a
// non-zero Exit makes the handle's Wait report a failure. A test that only
// needs a fault can leave ExecResults empty and set Faults.Exec instead.
type ExecResult struct {
	Match  string // substring matched against the exec command's last argument
	Stdout string
	Exit   int
}

// Runtime is the canonical fake. Construct it with New. Populate Containers
// (returned by List, matched by Inspect), Networks (returned by ListNetworks),
// ExecResults, and Faults directly; they are plain fields guarded by the fake's
// mutex on every access.
type Runtime struct {
	mu sync.Mutex

	// Containers is what List returns and what Inspect matches against, by ID
	// then Name. Networks is what ListNetworks returns, on top of any network
	// created through the Provisioner surface.
	Containers []runtime.Container
	Networks   []runtime.Network

	// ExecResults are consulted, in order, by Exec. Faults is the per-op error
	// injection surface. Both are safe to set before or between calls.
	ExecResults []ExecResult
	Faults      Faults

	// Clock is the fake clock the test threads into the code under test (for a
	// ballast daemon, into the scheduler). New installs a fresh one.
	Clock *Clock

	// event scripting
	events    chan runtime.Event
	errs      chan error
	watchOnce sync.Once

	// recordings for assertions
	execCmds                                []string
	createdNets, createdVols, createdConts  []string
	removedNets, removedVols, removedConts  []string
	pulled                                  []string
}

// New builds a Runtime with an installed fake Clock and buffered event and
// error channels ready for Watch. The buffers let a test Emit a burst of
// events before the watch loop is even running without blocking.
func New() *Runtime {
	return &Runtime{
		Clock:  NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Second),
		events: make(chan runtime.Event, 64),
		errs:   make(chan error, 8),
	}
}

// --- event scripting -------------------------------------------------------

// Emit queues a lifecycle event for the watch loop to observe. It never blocks
// past the channel buffer in practice; a full buffer is a test that scripted
// more events than it drained.
func (r *Runtime) Emit(ev runtime.Event) { r.events <- ev }

// Fail delivers a terminal error on Watch's error channel, the way a real
// adapter reports the socket dropping. It does not close the event stream.
func (r *Runtime) Fail(err error) { r.errs <- err }

// CloseWatch ends the event stream, so a watch loop selecting on it returns as
// it would when the socket closes. Safe to call once; a second call is a no-op.
func (r *Runtime) CloseWatch() {
	r.watchOnce.Do(func() { close(r.events) })
}

// --- runtime.Runtime -------------------------------------------------------

// List returns the configured containers, or Faults.List if set.
func (r *Runtime) List(context.Context) ([]runtime.Container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.List != nil {
		return nil, r.Faults.List
	}
	out := make([]runtime.Container, len(r.Containers))
	copy(out, r.Containers)
	return out, nil
}

// Inspect returns the container whose ID or Name matches id, or Faults.Inspect
// if set, or a not-found error.
func (r *Runtime) Inspect(_ context.Context, id string) (runtime.Container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.Inspect != nil {
		return runtime.Container{}, r.Faults.Inspect
	}
	for _, c := range r.Containers {
		if c.ID == id || c.Name == id {
			return c, nil
		}
	}
	return runtime.Container{}, fmt.Errorf("runtimetest: no such container %q", id)
}

// Watch returns the scripted event channel and error channel. If Faults.Watch
// is set it is delivered on the error channel immediately, before any scripted
// event, so a test can exercise a watch that fails at subscription time.
func (r *Runtime) Watch(context.Context) (<-chan runtime.Event, <-chan error) {
	r.mu.Lock()
	fault := r.Faults.Watch
	r.mu.Unlock()
	if fault != nil {
		r.errs <- fault
	}
	return r.events, r.errs
}

// Exec records the command, honors Faults.Exec, drains any stdin (mimicking the
// real adapter closing the write side after copying a dump in), then returns a
// handle whose Stdout and exit code come from the first matching ExecResult.
func (r *Runtime) Exec(_ context.Context, _ string, spec runtime.ExecSpec) (*runtime.ExecHandle, error) {
	if spec.Stdin != nil {
		_, _ = io.Copy(io.Discard, spec.Stdin)
	}

	cmd := ""
	if len(spec.Cmd) > 0 {
		cmd = spec.Cmd[len(spec.Cmd)-1]
	}

	r.mu.Lock()
	r.execCmds = append(r.execCmds, cmd)
	fault := r.Faults.Exec
	stdout, exit := "", 0
	for _, e := range r.ExecResults {
		if e.Match != "" && strings.Contains(cmd, e.Match) {
			stdout, exit = e.Stdout, e.Exit
			break
		}
	}
	r.mu.Unlock()

	if fault != nil {
		return nil, fault
	}
	return handleFor(stdout, exit), nil
}

func (r *Runtime) Stop(context.Context, string, int) error {
	return r.fault(func(f Faults) error { return f.Stop })
}
func (r *Runtime) Start(context.Context, string) error {
	return r.fault(func(f Faults) error { return f.Start })
}
func (r *Runtime) Kill(context.Context, string, string) error {
	return r.fault(func(f Faults) error { return f.Kill })
}
func (r *Runtime) Restart(context.Context, string) error {
	return r.fault(func(f Faults) error { return f.Restart })
}
func (r *Runtime) Close() error {
	return r.fault(func(f Faults) error { return f.Close })
}

func (r *Runtime) fault(pick func(Faults) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return pick(r.Faults)
}

func handleFor(stdout string, exit int) *runtime.ExecHandle {
	return &runtime.ExecHandle{
		Stdout: bytes.NewReader([]byte(stdout)),
		Wait: func() (int, error) {
			if exit != 0 {
				return exit, fmt.Errorf("runtimetest: command exited %d", exit)
			}
			return 0, nil
		},
	}
}

// --- runtime.Provisioner ---------------------------------------------------

func (r *Runtime) PullImage(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.PullImage != nil {
		return r.Faults.PullImage
	}
	r.pulled = append(r.pulled, ref)
	return nil
}

func (r *Runtime) CreateNetwork(_ context.Context, spec runtime.NetworkSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.CreateNetwork != nil {
		return "", r.Faults.CreateNetwork
	}
	r.createdNets = append(r.createdNets, spec.Name)
	return spec.Name, nil
}

func (r *Runtime) RemoveNetwork(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.RemoveNetwork != nil {
		return r.Faults.RemoveNetwork
	}
	r.removedNets = append(r.removedNets, id)
	return nil
}

func (r *Runtime) CreateVolume(_ context.Context, spec runtime.VolumeSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.CreateVolume != nil {
		return "", r.Faults.CreateVolume
	}
	r.createdVols = append(r.createdVols, spec.Name)
	return spec.Name, nil
}

func (r *Runtime) RemoveVolume(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.RemoveVolume != nil {
		return r.Faults.RemoveVolume
	}
	r.removedVols = append(r.removedVols, name)
	return nil
}

func (r *Runtime) CreateContainer(_ context.Context, spec runtime.ContainerSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.CreateContainer != nil {
		return "", r.Faults.CreateContainer
	}
	r.createdConts = append(r.createdConts, spec.Name)
	return spec.Name, nil
}

func (r *Runtime) RemoveContainer(_ context.Context, id string, _ bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.RemoveContainer != nil {
		return r.Faults.RemoveContainer
	}
	r.removedConts = append(r.removedConts, id)
	return nil
}

// --- runtime.NetworkInspector ----------------------------------------------

func (r *Runtime) ListNetworks(context.Context) ([]runtime.Network, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Faults.ListNetworks != nil {
		return nil, r.Faults.ListNetworks
	}
	out := make([]runtime.Network, 0, len(r.Networks)+len(r.createdNets))
	out = append(out, r.Networks...)
	for _, n := range r.createdNets {
		out = append(out, runtime.Network{Name: n, ID: n, Internal: true})
	}
	return out, nil
}

// --- recordings for assertions ---------------------------------------------

// ExecCommands returns the last argument of every Exec call, in order, so a
// test can assert which dump or quiesce commands ran.
func (r *Runtime) ExecCommands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.execCmds...)
}

// Pulled returns every image reference passed to PullImage, in order.
func (r *Runtime) Pulled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pulled...)
}

// Leaked reports every throwaway object created through the Provisioner surface
// that was not later removed, the teardown-completeness check a verify-style
// test asserts is empty.
func (r *Runtime) Leaked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var leaks []string
	count := func(kind string, created, removed []string) {
		rem := map[string]int{}
		for _, x := range removed {
			rem[x]++
		}
		for _, c := range created {
			if rem[c] == 0 {
				leaks = append(leaks, kind+":"+c)
				continue
			}
			rem[c]--
		}
	}
	count("network", r.createdNets, r.removedNets)
	count("volume", r.createdVols, r.removedVols)
	count("container", r.createdConts, r.removedConts)
	return leaks
}

// Compile-time assertions that the fake satisfies every interface a consumer
// might type-assert it to.
var (
	_ runtime.Runtime          = (*Runtime)(nil)
	_ runtime.Provisioner      = (*Runtime)(nil)
	_ runtime.NetworkInspector = (*Runtime)(nil)
)
