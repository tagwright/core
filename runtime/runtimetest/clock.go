// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package runtimetest

import (
	"sync"
	"time"
)

// Clock is a controllable fake clock. Its Now method is the func(s a consumer
// takes as its clock (a ballast Scheduler's SetClock, for instance), so a test
// drives time forward on its own terms rather than the wall clock's.
//
// Now auto-advances by a fixed step on every call. That single property is
// enough to make a poll loop like the scheduler's own fire deterministically:
// each read of "now" is a little later than the last, so a job whose next-fire
// time has passed comes due without the test having to interleave an explicit
// Advance between the scheduler's internal reads. Set the step to zero for a
// frozen clock the test advances only through Advance or Set.
type Clock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

// NewClock returns a Clock starting at start whose Now auto-advances by step on
// each call.
func NewClock(start time.Time, step time.Duration) *Clock {
	return &Clock{now: start, step: step}
}

// Now returns the current fake time, then advances it by the auto-step. It is
// the value to hand a consumer as its clock function.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.now
	c.now = c.now.Add(c.step)
	return t
}

// Advance moves the clock forward by d, on top of the auto-step, for a test
// that needs to jump time explicitly (past a debounce window, say).
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set pins the clock to t exactly, discarding any accumulated auto-step.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// SetStep changes the per-call auto-advance amount.
func (c *Clock) SetStep(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.step = step
}
