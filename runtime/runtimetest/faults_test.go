// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtimetest_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/tagwright/core/runtime"
	"github.com/tagwright/core/runtime/runtimetest"
)

// TestFaultKnobsSurfaceTable proves every remaining fault knob surfaces the
// exact sentinel it was set to. The four knobs already covered in
// runtimetest_test.go (List, Exec, Stop, Watch) are not repeated here. Each row
// sets one knob to a unique sentinel, calls the matching method on a fresh
// fake, and asserts the method returns that same sentinel by identity, so a
// knob accidentally wired to the wrong Faults field would fail rather than pass
// on any-non-nil.
func TestFaultKnobsSurfaceTable(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		set  func(f *runtimetest.Faults, err error)
		call func(rt *runtimetest.Runtime) error
	}{
		{
			name: "Inspect",
			set:  func(f *runtimetest.Faults, err error) { f.Inspect = err },
			call: func(rt *runtimetest.Runtime) error { _, err := rt.Inspect(ctx, "c"); return err },
		},
		{
			name: "Start",
			set:  func(f *runtimetest.Faults, err error) { f.Start = err },
			call: func(rt *runtimetest.Runtime) error { return rt.Start(ctx, "c") },
		},
		{
			name: "Kill",
			set:  func(f *runtimetest.Faults, err error) { f.Kill = err },
			call: func(rt *runtimetest.Runtime) error { return rt.Kill(ctx, "c", "SIGHUP") },
		},
		{
			name: "Restart",
			set:  func(f *runtimetest.Faults, err error) { f.Restart = err },
			call: func(rt *runtimetest.Runtime) error { return rt.Restart(ctx, "c") },
		},
		{
			name: "Close",
			set:  func(f *runtimetest.Faults, err error) { f.Close = err },
			call: func(rt *runtimetest.Runtime) error { return rt.Close() },
		},
		{
			name: "PullImage",
			set:  func(f *runtimetest.Faults, err error) { f.PullImage = err },
			call: func(rt *runtimetest.Runtime) error { return rt.PullImage(ctx, "img:tag") },
		},
		{
			name: "CreateNetwork",
			set:  func(f *runtimetest.Faults, err error) { f.CreateNetwork = err },
			call: func(rt *runtimetest.Runtime) error {
				_, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: "n"})
				return err
			},
		},
		{
			name: "RemoveNetwork",
			set:  func(f *runtimetest.Faults, err error) { f.RemoveNetwork = err },
			call: func(rt *runtimetest.Runtime) error { return rt.RemoveNetwork(ctx, "n") },
		},
		{
			name: "CreateVolume",
			set:  func(f *runtimetest.Faults, err error) { f.CreateVolume = err },
			call: func(rt *runtimetest.Runtime) error {
				_, err := rt.CreateVolume(ctx, runtime.VolumeSpec{Name: "v"})
				return err
			},
		},
		{
			name: "RemoveVolume",
			set:  func(f *runtimetest.Faults, err error) { f.RemoveVolume = err },
			call: func(rt *runtimetest.Runtime) error { return rt.RemoveVolume(ctx, "v") },
		},
		{
			name: "CreateContainer",
			set:  func(f *runtimetest.Faults, err error) { f.CreateContainer = err },
			call: func(rt *runtimetest.Runtime) error {
				_, err := rt.CreateContainer(ctx, runtime.ContainerSpec{Name: "c", Image: "img:tag"})
				return err
			},
		},
		{
			name: "RemoveContainer",
			set:  func(f *runtimetest.Faults, err error) { f.RemoveContainer = err },
			call: func(rt *runtimetest.Runtime) error { return rt.RemoveContainer(ctx, "c", false) },
		},
		{
			name: "ListNetworks",
			set:  func(f *runtimetest.Faults, err error) { f.ListNetworks = err },
			call: func(rt *runtimetest.Runtime) error { _, err := rt.ListNetworks(ctx); return err },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := errors.New("injected " + tc.name)
			rt := runtimetest.New()
			tc.set(&rt.Faults, sentinel)
			if err := tc.call(rt); !errors.Is(err, sentinel) {
				t.Fatalf("%s: want injected sentinel, got %v", tc.name, err)
			}
		})
	}
}

// TestExecResultNonZeroExitFailsWait proves the case the standard singles out:
// a happy-path handle whose Wait always reports success does not count. An
// ExecResult with a non-zero Exit must make the returned handle's Wait report a
// non-nil error carrying that exit code, so a wiring test that only checks err
// on Exec itself is not enough and the exit path is exercised.
func TestExecResultNonZeroExitFailsWait(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	rt.ExecResults = []runtimetest.ExecResult{{Match: "pg_dump", Stdout: "", Exit: 3}}

	h, err := rt.Exec(ctx, "c", runtime.ExecSpec{Cmd: []string{"sh", "-c", "pg_dump db"}})
	if err != nil {
		t.Fatalf("Exec: unexpected error %v", err)
	}
	// Drain stdout the way a real caller does before Wait.
	_, _ = io.ReadAll(h.Stdout)

	code, werr := h.Wait()
	if code != 3 {
		t.Fatalf("Wait: want exit code 3, got %d", code)
	}
	if werr == nil {
		t.Fatal("Wait: want a non-nil error for a non-zero exit, got nil")
	}
}

// TestFailDeliversMidStream proves Fail is distinct from Faults.Watch. Where
// Faults.Watch fires at subscribe time, Fail delivers a terminal error on the
// error channel mid-stream, the way a real adapter reports the socket dropping
// after events have already flowed. The consumer must see the emitted event
// first, then the injected error.
func TestFailDeliversMidStream(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	events, errs := rt.Watch(ctx)

	rt.Emit(runtime.Event{Type: runtime.EventStart, ID: "c1", Name: "svc"})
	sentinel := errors.New("socket dropped")
	rt.Fail(sentinel)

	select {
	case ev := <-events:
		if ev.Type != runtime.EventStart || ev.ID != "c1" {
			t.Fatalf("first read: want start/c1, got %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the emitted event first, got nothing")
	}

	select {
	case err := <-errs:
		if !errors.Is(err, sentinel) {
			t.Fatalf("mid-stream error: want injected sentinel, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected the mid-stream error, got nothing")
	}
}

// TestCloseWatchEndsStream proves CloseWatch ends the event stream so a
// consumer selecting on it observes the close (a zero value with ok false),
// the way a watch loop returns when the socket closes.
func TestCloseWatchEndsStream(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	events, _ := rt.Watch(ctx)

	rt.CloseWatch()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("want the event channel closed, got a live value")
		}
	case <-time.After(time.Second):
		t.Fatal("expected the closed event channel to be observable, got a block")
	}
}

// TestLeakedContainerAccounting rounds out the teardown-completeness accounting
// for the third provisioned object kind (the existing suite covers network and
// volume). A created container shows as leaked until removed, then the ledger
// is clean, which is the invariant a verify-style test asserts is empty.
func TestLeakedContainerAccounting(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()

	if _, err := rt.CreateContainer(ctx, runtime.ContainerSpec{Name: "c1", Image: "img:tag"}); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	leaks := rt.Leaked()
	if len(leaks) != 1 || leaks[0] != "container:c1" {
		t.Fatalf("want one leaked container c1, got %v", leaks)
	}

	if err := rt.RemoveContainer(ctx, "c1", false); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if leaks := rt.Leaked(); len(leaks) != 0 {
		t.Fatalf("want a clean ledger after removal, got %v", leaks)
	}
}
