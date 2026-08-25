// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/ktime"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
)

func TestSetDilationRational(t *testing.T) {
	for _, tc := range []struct {
		factor   float64
		wantNum  uint64
		wantDen  uint64
		enabled  bool
	}{
		{1.0, 1, 1, false},
		{0, 1, 1, false},
		{-3, 1, 1, false},
		{6.0, 6, 1, true},
		{2.5, 5, 2, true},
		{6.001, 6001, 1000, true},
	} {
		tk := NewTimekeeper()
		tk.SetDilation(tc.factor, 0)
		if tk.dilationNum != tc.wantNum || tk.dilationDen != tc.wantDen {
			t.Errorf("SetDilation(%v): got %d/%d, want %d/%d", tc.factor, tk.dilationNum, tk.dilationDen, tc.wantNum, tc.wantDen)
		}
		if tk.dilationEnabled() != tc.enabled {
			t.Errorf("SetDilation(%v): enabled = %v, want %v", tc.factor, tk.dilationEnabled(), tc.enabled)
		}
	}
}

func TestDilateNS(t *testing.T) {
	tk := NewTimekeeper()
	tk.SetDilation(6, 0)
	for _, tc := range []struct {
		in   int64
		want int64
	}{
		{0, 0},
		{10, 60},
		{-10, -60},
		{time.Hour.Nanoseconds(), 6 * time.Hour.Nanoseconds()},
		// ~1 year of host nanoseconds must not overflow at 6x.
		{365 * 24 * time.Hour.Nanoseconds(), 6 * 365 * 24 * time.Hour.Nanoseconds()},
	} {
		if got := tk.dilateNS(tc.in); got != tc.want {
			t.Errorf("dilateNS(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// Non-integer factor exercises the 128-bit path with a large input.
	tk2 := NewTimekeeper()
	tk2.SetDilation(6.001, 0)
	in := 365 * 24 * time.Hour.Nanoseconds()
	want := int64(float64(in) * 6.001)
	got := tk2.dilateNS(in)
	if diff := got - want; diff < -1000 || diff > 1000 {
		t.Errorf("dilateNS(%d) at 6.001x = %d, want ~%d", in, got, want)
	}
}

func TestDilateFrequency(t *testing.T) {
	tk := NewTimekeeper()
	tk.SetDilation(6, 0)
	// 24 MHz (typical arm64 CNTFRQ) at 6x -> 4 MHz effective.
	if got := tk.dilateFrequency(24_000_000); got != 4_000_000 {
		t.Errorf("dilateFrequency(24MHz) = %d, want 4000000", got)
	}
	// Frequency must never reach 0.
	if got := tk.dilateFrequency(3); got == 0 {
		t.Error("dilateFrequency(3) = 0, want >= 1")
	}
}

func TestWallTimeUntilDilated(t *testing.T) {
	tk := NewTimekeeper()
	tk.SetDilation(6, 0)
	tc := &timekeeperClock{tk: tk, c: sentrytime.Monotonic}

	now := ktime.FromNanoseconds(0)
	deadline := ktime.FromNanoseconds((60 * time.Second).Nanoseconds())
	got := tc.WallTimeUntil(deadline, now)
	want := 10 * time.Second
	if got != want {
		t.Errorf("WallTimeUntil(60s ahead) = %v, want %v", got, want)
	}

	// Undilated timekeeper passes durations through.
	tk1 := NewTimekeeper()
	tc1 := &timekeeperClock{tk: tk1, c: sentrytime.Monotonic}
	if got := tc1.WallTimeUntil(deadline, now); got != 60*time.Second {
		t.Errorf("undilated WallTimeUntil = %v, want 60s", got)
	}

	// A zeroed timekeeper (restore path: dilation fields are not saved)
	// must behave as undilated, not divide by zero.
	tkRestored := &Timekeeper{}
	tcR := &timekeeperClock{tk: tkRestored, c: sentrytime.Monotonic}
	if got := tcR.WallTimeUntil(deadline, now); got != 60*time.Second {
		t.Errorf("restored WallTimeUntil = %v, want 60s", got)
	}
	if tkRestored.dilationEnabled() {
		t.Error("zeroed Timekeeper reports dilation enabled")
	}
}

func TestSharedEpochAnchor(t *testing.T) {
	// Two sandboxes with the same factor and epoch must map any host
	// realtime instant to the same dilated instant, regardless of when
	// each "booted" (i.e. what rtAnchorNS would have been without the
	// epoch).
	epoch := int64(1_700_000_000_000_000_000)
	a := NewTimekeeper()
	a.SetDilation(6, epoch)
	a.rtAnchorNS = a.dilationEpochNS
	b := NewTimekeeper()
	b.SetDilation(6, epoch)
	b.rtAnchorNS = b.dilationEpochNS

	for _, hostNS := range []int64{
		epoch,
		epoch + time.Minute.Nanoseconds(),
		epoch + 30*24*time.Hour.Nanoseconds(),
	} {
		if ga, gb := a.dilateRealtime(hostNS), b.dilateRealtime(hostNS); ga != gb {
			t.Errorf("dilateRealtime(%d): a=%d b=%d, want equal", hostNS, ga, gb)
		}
	}
	// The epoch itself is the fixed point.
	if got := a.dilateRealtime(epoch); got != epoch {
		t.Errorf("dilateRealtime(epoch) = %d, want %d", got, epoch)
	}
	// One minute after epoch maps to six minutes after epoch.
	if got := a.dilateRealtime(epoch + time.Minute.Nanoseconds()); got != epoch+6*time.Minute.Nanoseconds() {
		t.Errorf("dilateRealtime(epoch+1m) = %d, want epoch+6m", got)
	}
}
