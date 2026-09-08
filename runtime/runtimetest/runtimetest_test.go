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

func TestFaultKnobsSurface(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("injected")

	t.Run("List", func(t *testing.T) {
		rt := runtimetest.New()
		rt.Faults.List = sentinel
		if _, err := rt.List(ctx); !errors.Is(err, sentinel) {
			t.Fatalf("List: want injected error, got %v", err)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		rt := runtimetest.New()
		rt.Faults.Exec = sentinel
		if _, err := rt.Exec(ctx, "c", runtime.ExecSpec{Cmd: []string{"sh", "-c", "dump"}}); !errors.Is(err, sentinel) {
			t.Fatalf("Exec: want injected error, got %v", err)
		}
	})

	t.Run("Stop", func(t *testing.T) {
		rt := runtimetest.New()
		rt.Faults.Stop = sentinel
		if err := rt.Stop(ctx, "c", 10); !errors.Is(err, sentinel) {
			t.Fatalf("Stop: want injected error, got %v", err)
		}
	})

	t.Run("Watch delivers fault on the error channel", func(t *testing.T) {
		rt := runtimetest.New()
		rt.Faults.Watch = sentinel
		_, errs := rt.Watch(ctx)
		select {
		case err := <-errs:
			if !errors.Is(err, sentinel) {
				t.Fatalf("Watch: want injected error, got %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Watch: expected an error on the channel, got none")
		}
	})
}

func TestHappyPathHasNoFault(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	rt.Containers = []runtime.Container{{ID: "c1", Name: "svc"}}

	got, err := rt.List(ctx)
	if err != nil {
		t.Fatalf("List: unexpected error %v", err)
	}
	if len(got) != 1 || got[0].ID != "c1" {
		t.Fatalf("List: want one container c1, got %v", got)
	}

	c, err := rt.Inspect(ctx, "svc")
	if err != nil {
		t.Fatalf("Inspect by name: unexpected error %v", err)
	}
	if c.ID != "c1" {
		t.Fatalf("Inspect: want c1, got %q", c.ID)
	}
}

func TestExecResultMatch(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	rt.ExecResults = []runtimetest.ExecResult{{Match: "pg_isready", Stdout: "accepting connections\n", Exit: 0}}

	h, err := rt.Exec(ctx, "c", runtime.ExecSpec{Cmd: []string{"sh", "-c", "pg_isready -U postgres"}})
	if err != nil {
		t.Fatalf("Exec: unexpected error %v", err)
	}
	out, _ := io.ReadAll(h.Stdout)
	if string(out) != "accepting connections\n" {
		t.Fatalf("Exec stdout: got %q", out)
	}
	if code, err := h.Wait(); code != 0 || err != nil {
		t.Fatalf("Exec wait: want (0,nil), got (%d,%v)", code, err)
	}
}

func TestScriptedEvents(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	events, _ := rt.Watch(ctx)

	rt.Emit(runtime.Event{Type: runtime.EventStart, ID: "c1", Name: "svc"})
	rt.CloseWatch()

	var got []runtime.Event
	for ev := range events {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Type != runtime.EventStart || got[0].ID != "c1" {
		t.Fatalf("scripted events: got %v", got)
	}
}

func TestClockAdvances(t *testing.T) {
	c := runtimetest.NewClock(time.Unix(0, 0).UTC(), time.Minute)
	t0 := c.Now()
	t1 := c.Now()
	if !t1.After(t0) {
		t.Fatalf("clock did not auto-advance: t0=%v t1=%v", t0, t1)
	}
	if d := t1.Sub(t0); d != time.Minute {
		t.Fatalf("auto-step: want 1m, got %v", d)
	}
	c.Advance(time.Hour)
	if d := c.Now().Sub(t1); d < time.Hour {
		t.Fatalf("Advance did not jump the clock forward, delta=%v", d)
	}
}

func TestLeakedTracksProvisioning(t *testing.T) {
	ctx := context.Background()
	rt := runtimetest.New()
	if _, err := rt.CreateNetwork(ctx, runtime.NetworkSpec{Name: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.CreateVolume(ctx, runtime.VolumeSpec{Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if leaks := rt.Leaked(); len(leaks) != 2 {
		t.Fatalf("want two leaks before teardown, got %v", leaks)
	}
	_ = rt.RemoveNetwork(ctx, "n1")
	_ = rt.RemoveVolume(ctx, "v1")
	if leaks := rt.Leaked(); len(leaks) != 0 {
		t.Fatalf("want no leaks after teardown, got %v", leaks)
	}
}
