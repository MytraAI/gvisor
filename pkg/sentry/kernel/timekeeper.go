// Copyright 2018 The gVisor Authors.
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
	"fmt"
	"math/bits"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// Possible values of Timekeeper.updaterState
const (
	// manually stopped
	updaterStopped int32 = iota

	// timer not armed due to being idle for a whole interval
	updaterParked

	// set when timer triggers if no tasks are running; any task running
	// or internal timekeeper usage transitions back to updaterActive
	updaterIdle

	updaterActive
)

// Timekeeper manages all of the kernel clocks.
//
// +stateify savable
type Timekeeper struct {
	// clocks are the clock sources.
	//
	// These are not saved directly, as the new machine's clock may behave
	// differently.
	//
	// It is set only once, by SetClocks.
	clocks sentrytime.Clocks `state:"nosave"`

	// realtimeClock is a ktime.Clock based on timekeeper's Realtime.
	realtimeClock *timekeeperClock

	// monotonicClock is a ktime.Clock based on timekeeper's Monotonic.
	monotonicClock *timekeeperClock

	// monotonicRawClock is a ktime.Clock based on timekeeper's MonotonicRaw.
	// It is non-nil only when the clock source tracks a distinct
	// CLOCK_MONOTONIC_RAW; otherwise CLOCK_MONOTONIC_RAW aliases
	// CLOCK_MONOTONIC. It is derived by SetClocks, anew on restore.
	monotonicRawClock *timekeeperClock `state:"nosave"`

	// bootTime is the realtime when the system "booted". i.e., when
	// SetClocks was called in the initial (not restored) run.
	bootTime ktime.Time

	// monotonicOffset is the offset to apply to the monotonic clock output
	// from clocks.
	//
	// It is set only once, by SetClocks.
	monotonicOffset int64 `state:"nosave"`

	// monotonicLowerBound is the lowerBound for monotonic time.
	monotonicLowerBound atomicbitops.Int64 `state:"nosave"`

	// restored, if non-nil, indicates that this Timekeeper was restored
	// from a state file. The clocks are not set until restored is closed.
	restored chan struct{} `state:"nosave"`

	// saveMonotonic is the (offset) value of the monotonic clock at the
	// time of save.
	//
	// It is only valid if restored is non-nil.
	//
	// It is only used in SetClocks after restore to compute the new
	// monotonicOffset.
	saveMonotonic int64

	// saveRealtime is the value of the realtime clock at the time of save.
	//
	// It is only valid if restored is non-nil.
	//
	// It is only used in SetClocks after restore to compute the new
	// monotonicOffset.
	saveRealtime int64

	// mu protects destruction with stop and wg.
	mu sync.Mutex `state:"nosave"`

	// stop is used to tell the update goroutine to exit.
	stop chan struct{} `state:"nosave"`

	// wg is used to indicate that the update goroutine has exited.
	wg sync.WaitGroup `state:"nosave"`

	// atomic enum with updater* values
	//
	// the goroutine moves it active->idle->parked (under updateMu)
	// GetTime or 0->1 tasks moves to active if not stopped (lockless)
	// startUpdater/stopUpdater move in/out of stopped (under updateMu)
	updaterState atomicbitops.Int32 `state:"nosave"`

	// (runningTasks > 0 ? 1 : 0) + number_of_tasks_running_GetTime
	refs atomicbitops.Int64 `state:"nosave"`

	// guards updates of the vDSO parameter page and of updaterState
	// except for raising updaterState to active on usage
	updateMu sync.Mutex `state:"nosave"`

	// timer to periodically update the time calibration
	timer *time.Timer `state:"nosave"`

	// vDSO parameter page kept updated by the Timekeeper mechanism
	params *VDSOParamPage `state:"nosave"`

	// dilationNum and dilationDen express the sandbox time dilation factor
	// as a rational number: app-visible clocks advance dilationNum
	// nanoseconds per dilationDen host nanoseconds. Both are 1 when
	// dilation is disabled. Set at most once, by SetDilation, before
	// SetClocks; not saved, so a restored sandbox must have SetDilation
	// re-applied (from runsc config) before SetClocks.
	dilationNum uint64 `state:"nosave"`
	dilationDen uint64 `state:"nosave"`

	// dilationFloat is the same factor as a float64, used to convert
	// dilated durations back to host wall durations for timer arming.
	dilationFloat float64 `state:"nosave"`

	// rawBootNS is the value of the backing CLOCK_MONOTONIC_RAW when
	// SetClocks ran; it anchors dilation of MonotonicRaw.
	rawBootNS int64 `state:"nosave"`

	// dilationEpochNS, if nonzero, is a fixed host CLOCK_REALTIME instant
	// (in nanoseconds) used as the anchor of the realtime dilation
	// transform. Sandboxes sharing the same epoch and factor agree on
	// wall-clock time exactly, regardless of when each booted. Zero means
	// anchor at this sandbox's own boot (the sandbox starts at host wall
	// time but skews from other sandboxes by (N-1) x boot-time spread).
	dilationEpochNS int64 `state:"nosave"`

	// rtAnchorNS is the resolved realtime dilation anchor: dilationEpochNS
	// if set, else the host realtime sampled at SetClocks. Set by
	// SetClocks.
	rtAnchorNS int64 `state:"nosave"`
}

// NewTimekeeper returns a Timekeeper that is automatically kept up-to-date.
// NewTimekeeper does not take ownership of paramPage.
//
// SetClocks must be called on the returned Timekeeper before it is usable.
func NewTimekeeper() *Timekeeper {
	t := Timekeeper{
		dilationNum:   1,
		dilationDen:   1,
		dilationFloat: 1.0,
	}
	t.realtimeClock = &timekeeperClock{tk: &t, c: sentrytime.Realtime}
	t.monotonicClock = &timekeeperClock{tk: &t, c: sentrytime.Monotonic}
	return &t
}

// SetDilation sets the sandbox time dilation factor: all app-visible clocks
// (and therefore all timers, sleeps, and timeouts) advance factor times
// faster than host time. Realtime is anchored at boot time, so the sandbox
// starts at the host wall time and diverges from it as the sandbox runs.
//
// SetDilation must be called before SetClocks (including after restore) and
// at most once. A factor of 1 (or <= 0) disables dilation.
//
// epochNS, if nonzero, is a fixed host CLOCK_REALTIME instant (nanoseconds)
// anchoring the realtime transform; all sandboxes given the same factor and
// epoch agree on wall-clock time exactly. Zero anchors at this sandbox's own
// boot.
func (t *Timekeeper) SetDilation(factor float64, epochNS int64) {
	if t.clocks != nil {
		panic("SetDilation called after SetClocks")
	}
	if factor <= 0 || factor == 1.0 {
		return
	}
	if epochNS > 0 {
		t.dilationEpochNS = epochNS
	}
	// Express the factor as a rational with millesimal precision so clock
	// arithmetic stays exact and monotone.
	num := uint64(factor*1000 + 0.5)
	den := uint64(1000)
	if num == 0 {
		return
	}
	for d := gcd(num, den); d > 1; d = gcd(num, den) {
		num /= d
		den /= d
	}
	t.dilationNum = num
	t.dilationDen = den
	t.dilationFloat = float64(num) / float64(den)
	log.Infof("Timekeeper: sandbox time dilation enabled: %d/%d (%v)", num, den, t.dilationFloat)
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// dilationEnabled returns true if a non-1 dilation factor is set.
func (t *Timekeeper) dilationEnabled() bool {
	return t.dilationNum != t.dilationDen
}

// dilateNS scales a (host-relative) nanosecond delta into dilated sandbox
// nanoseconds using 128-bit intermediate math, so large deltas and
// non-integer factors cannot overflow.
func (t *Timekeeper) dilateNS(ns int64) int64 {
	neg := false
	v := uint64(ns)
	if ns < 0 {
		neg = true
		v = uint64(-ns)
	}
	hi, lo := bits.Mul64(v, t.dilationNum)
	// The quotient always fits: dilated results are themselves int64
	// nanoseconds, so hi < dilationDen holds for all reachable inputs.
	q, _ := bits.Div64(hi, lo, t.dilationDen)
	r := int64(q)
	if neg {
		r = -r
	}
	return r
}

// dilateRealtime moves a host CLOCK_REALTIME sample onto the dilated
// timeline, anchored at rtAnchorNS.
func (t *Timekeeper) dilateRealtime(ns int64) int64 {
	return t.rtAnchorNS + t.dilateNS(ns-t.rtAnchorNS)
}

// dilateFrequency scales a cycle-counter frequency down by the dilation
// factor, which makes vDSO time computed from raw cycles advance
// dilationNum/dilationDen times faster.
func (t *Timekeeper) dilateFrequency(freq uint64) uint64 {
	f := freq * t.dilationDen / t.dilationNum
	if f == 0 {
		f = 1
	}
	return f
}

// SetClocks the backing clock source.
//
// SetClocks must be called before the Timekeeper is used, and it may not be
// called more than once, as changing the clock source without extra correction
// could cause time discontinuities.
//
// It must also be called after Load.
func (t *Timekeeper) SetClocks(c sentrytime.Clocks, params *VDSOParamPage) {
	// Update the params, marking them "not ready", as we may need to
	// restart calibration on this new machine.
	if t.restored != nil {
		if err := params.Write(func() vdsoParams {
			return vdsoParams{}
		}); err != nil {
			panic("unable to reset VDSO params: " + err.Error())
		}
	}

	if t.clocks != nil {
		panic("SetClocks called on previously-initialized Timekeeper")
	}

	t.clocks = c

	// Serve CLOCK_MONOTONIC_RAW as a distinct clock iff the clock source
	// tracks it (see NewCalibratedClocks). Deriving this from the source keeps
	// boot and restore coherent: a restored sandbox follows its current
	// configuration, not the checkpointed one.
	if c.MonotonicRawEnabled() {
		t.monotonicRawClock = &timekeeperClock{tk: t, c: sentrytime.MonotonicRaw}
		if t.dilationEnabled() {
			nowRaw, err := t.clocks.GetTime(sentrytime.MonotonicRaw)
			if err != nil {
				panic("Unable to get current monotonic raw time: " + err.Error())
			}
			t.rawBootNS = nowRaw
		}
	}

	// Compute the offset of the monotonic clock from the base Clocks.
	//
	// In a fresh (not restored) sentry, monotonic time starts at zero.
	//
	// In a restored sentry, monotonic time jumps forward by approximately
	// the same amount as real time. There are no guarantees here, we are
	// just making a best-effort attempt to make it appear that the app
	// was simply not scheduled for a long period, rather than that the
	// real time clock was changed.
	//
	// If real time went backwards, it remains the same.
	wantMonotonic := int64(0)

	nowMonotonic, err := t.clocks.GetTime(sentrytime.Monotonic)
	if err != nil {
		panic("Unable to get current monotonic time: " + err.Error())
	}

	nowRealtime, err := t.clocks.GetTime(sentrytime.Realtime)
	if err != nil {
		panic("Unable to get current realtime: " + err.Error())
	}

	if t.restored != nil {
		wantMonotonic = t.saveMonotonic
		elapsed := nowRealtime - t.saveRealtime
		if elapsed > 0 {
			wantMonotonic += elapsed
		}
	}

	t.monotonicOffset = wantMonotonic - nowMonotonic

	if t.dilationEnabled() {
		if t.dilationEpochNS > 0 {
			t.rtAnchorNS = t.dilationEpochNS
		} else {
			t.rtAnchorNS = nowRealtime
		}
	}

	if t.restored == nil {
		// Hold on to the initial "boot" time, expressed on the dilated
		// timeline so /proc btime, starttime, etc. stay coherent with
		// the realtime clock apps observe. With a boot anchor this is
		// the identity; with an epoch anchor boot lands ahead of host
		// wall time.
		bootNS := nowRealtime
		if t.dilationEnabled() {
			bootNS = t.dilateRealtime(nowRealtime)
		}
		t.bootTime = ktime.FromNanoseconds(bootNS)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.startUpdater(params)

	if t.restored != nil {
		close(t.restored)
	}
}

// update samples the backing clocks and writes the new parameters to the VDSO
// parameter page.
//
// Preconditions: updateMu must be held
func (t *Timekeeper) update(parked bool) {
	// Call Update within a Write block to prevent the VDSO from using the old
	// params between Update and Write.
	if err := t.params.Write(func() vdsoParams {
		res := t.clocks.Update(parked)

		var p vdsoParams
		if res.MonotonicOk {
			p.monotonicReady = 1
			p.monotonicBaseCycles = int64(res.Monotonic.BaseCycles)
			p.monotonicBaseRef = int64(res.Monotonic.BaseRef) + t.monotonicOffset
			p.monotonicFrequency = res.Monotonic.Frequency
		}
		if res.RealtimeOk {
			p.realtimeReady = 1
			p.realtimeBaseCycles = int64(res.Realtime.BaseCycles)
			p.realtimeBaseRef = int64(res.Realtime.BaseRef)
			p.realtimeFrequency = res.Realtime.Frequency
		}
		if t.monotonicRawClock == nil {
			p.monotonicRawAlias = 1
		} else if res.MonotonicRawOk {
			// Raw tracks absolute host CLOCK_MONOTONIC_RAW, so unlike
			// monotonic its base ref is published without monotonicOffset.
			p.monotonicRawReady = 1
			p.monotonicRawBaseCycles = int64(res.MonotonicRaw.BaseCycles)
			p.monotonicRawBaseRef = int64(res.MonotonicRaw.BaseRef)
			p.monotonicRawFrequency = res.MonotonicRaw.Frequency
		}
		if t.dilationEnabled() {
			// Dilate the params the vDSO computes time from:
			// compute_time = base_ref + delta_cycles * 1e9 / frequency,
			// so scaling frequency down scales the rate up, and the
			// base refs are moved onto the dilated timeline. Monotonic
			// is anchored at 0 (base ref already includes
			// monotonicOffset), realtime at bootTime, raw at its value
			// when SetClocks ran.
			if p.monotonicReady != 0 {
				p.monotonicBaseRef = t.dilateNS(p.monotonicBaseRef)
				p.monotonicFrequency = t.dilateFrequency(p.monotonicFrequency)
			}
			if p.realtimeReady != 0 {
				p.realtimeBaseRef = t.dilateRealtime(p.realtimeBaseRef)
				p.realtimeFrequency = t.dilateFrequency(p.realtimeFrequency)
			}
			if p.monotonicRawReady != 0 {
				p.monotonicRawBaseRef = t.rawBootNS + t.dilateNS(p.monotonicRawBaseRef-t.rawBootNS)
				p.monotonicRawFrequency = t.dilateFrequency(p.monotonicRawFrequency)
			}
		}
		return p
	}); err != nil {
		log.Warningf("Unable to update VDSO parameter page: %v", err)
	}
}

// startUpdater starts an update goroutine that keeps the clocks updated.
//
// mu must be held.
func (t *Timekeeper) startUpdater(params *VDSOParamPage) {
	if t.stop != nil {
		// Timekeeper already started
		return
	}
	t.stop = make(chan struct{})
	t.params = params

	// Keep the clocks up to date.
	//
	// Note that the Go runtime uses host CLOCK_MONOTONIC to service the
	// timer, so it may run at a *slightly* different rate from the
	// application CLOCK_MONOTONIC. That is fine, as we only need to update
	// at approximately this rate.
	t.timer = time.NewTimer(sentrytime.ApproxUpdateInterval)
	t.timer.Stop()

	t.updateMu.Lock()
	// store-then-load to synchronize with addRef
	t.updaterState.Store(updaterParked)
	if t.refs.Load() != 0 {
		t.timer.Reset(sentrytime.ApproxUpdateInterval)
		t.update(true)
		t.updaterState.Store(updaterActive)
	}
	t.updateMu.Unlock()

	t.wg.Add(1)
	go func() { // S/R-SAFE: stopped during save.
		defer t.wg.Done()
		defer t.timer.Stop()
		for {
			// wait until next tick or if parked until addRef is called
			select {
			case <-t.timer.C:
			case <-t.stop:
				return
			}

			t.updateMu.Lock()

			switch t.updaterState.Load() {
			case updaterStopped:
				t.updateMu.Unlock()
				return

			case updaterActive:
				// store-then-load to synchronize with addRef
				t.updaterState.Store(updaterIdle)
				if t.refs.Load() != 0 {
					t.updaterState.Store(updaterActive)
				}

			case updaterIdle:
				// no Timekeeper usage for a whole interval: park
				// store-then-load to synchronize with addRef
				t.updaterState.Store(updaterParked)
				if t.refs.Load() != 0 {
					t.updaterState.Store(updaterActive)
				} else {
					t.updateMu.Unlock()
					continue
				}
			}

			t.timer.Reset(sentrytime.ApproxUpdateInterval)
			t.update(false)
			t.updateMu.Unlock()
		}
	}()
}

// add a reference to the Timekeeper: as long as there are references,
// it doesn't park. Call release() to release.
func (t *Timekeeper) addRef() {
	// store-then-load to synchronize with the goroutine
	t.refs.Add(1)
	switch t.updaterState.Load() {
	case updaterActive:
	case updaterIdle:
		// this is enough because the goroutine will
		// itself recheck refs after parking
		t.updaterState.CompareAndSwap(updaterIdle, updaterActive)
	case updaterParked:
		t.updateMu.Lock()
		cur := t.updaterState.Load()
		if cur == updaterParked {
			t.timer.Reset(sentrytime.ApproxUpdateInterval)
			t.update(true)
		}
		if cur != updaterStopped {
			t.updaterState.Store(updaterActive)
		}
		t.updateMu.Unlock()
	case updaterStopped:
	}
}

// release drops a reference taken by addRef; when refs are 0 the timekeeper can park
func (t *Timekeeper) release() {
	r := t.refs.Add(-1)
	if r < 0 {
		panic("Timekeeper.release called with no reference held")
	}
}

// stopUpdater stops the update goroutine, blocking until it exits.
//
// mu must be held.
func (t *Timekeeper) stopUpdater() {
	if t.stop == nil {
		// Updater not running.
		return
	}

	// lock to ensure it can't be taken out of stopped by non-startUpdater code
	t.updateMu.Lock()
	t.updaterState.Store(updaterStopped)
	t.updateMu.Unlock()

	// this wakes up the goroutine
	close(t.stop)
	t.wg.Wait()
	t.stop = nil
	t.timer = nil
	t.params = nil
}

// Destroy destroys the Timekeeper, freeing all associated resources.
func (t *Timekeeper) Destroy() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.stopUpdater()
}

// PauseUpdates stops clock parameter updates. This should only be used when
// Tasks are not running and thus cannot access the clock and when
// GetTime is not being called internally
func (t *Timekeeper) PauseUpdates() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopUpdater()
}

// ResumeUpdates restarts clock parameter updates stopped by PauseUpdates.
func (t *Timekeeper) ResumeUpdates(params *VDSOParamPage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startUpdater(params)
}

// GetTime returns the current time in nanoseconds.
func (t *Timekeeper) GetTime(c sentrytime.ClockID) (int64, error) {
	if t.clocks == nil {
		if t.restored == nil {
			panic("Timekeeper used before initialized with SetClocks")
		}
		<-t.restored
	}
	if c == sentrytime.MonotonicRaw && t.monotonicRawClock == nil {
		c = sentrytime.Monotonic
	}

	// update the calibration if needed and keep the timekeeper calibrated
	// during the read
	t.addRef()
	defer t.release()

	now, err := t.clocks.GetTime(c)
	if err == nil && t.dilationEnabled() {
		// Move the sample onto the dilated timeline, using the same
		// anchors as update() so the syscall and vDSO paths agree.
		switch c {
		case sentrytime.Monotonic:
			// Handled below: dilation composes with monotonicOffset.
		case sentrytime.Realtime:
			now = t.dilateRealtime(now)
		case sentrytime.MonotonicRaw:
			now = t.rawBootNS + t.dilateNS(now-t.rawBootNS)
		}
	}
	if err == nil && c == sentrytime.Monotonic {
		now += t.monotonicOffset
		if t.dilationEnabled() {
			now = t.dilateNS(now)
		}
		for {
			// It's possible that the clock is shaky. This may be due to
			// platform issues, e.g. the KVM platform relies on the guest
			// TSC and host TSC, which may not be perfectly in sync. To
			// work around this issue, ensure that the monotonic time is
			// always bounded by the last time read.
			oldLowerBound := t.monotonicLowerBound.Load()
			if now < oldLowerBound {
				now = oldLowerBound
				break
			}
			if t.monotonicLowerBound.CompareAndSwap(oldLowerBound, now) {
				break
			}
		}
	}
	return now, err
}

// BootTime returns the system boot real time.
func (t *Timekeeper) BootTime() ktime.Time {
	return t.bootTime
}

// timekeeperClock is a ktime.SampledClock that reads time from a
// kernel.Timekeeper-managed clock.
//
// +stateify savable
type timekeeperClock struct {
	tk *Timekeeper
	c  sentrytime.ClockID

	// Implements waiter.Waitable. (We have no ability to detect
	// discontinuities from external changes to CLOCK_REALTIME).
	ktime.NoClockEvents `state:"nosave"`
}

// WallTimeUntil implements ktime.SampledClock.WallTimeUntil.
//
// Deadlines are in dilated sandbox time, but the kicker timers that wake
// SampledTimer goroutines sleep in host wall time, so the remaining duration
// must be converted back. Float rounding is fine here: SampledTimer
// re-samples the clock on every wakeup, so an early wakeup just re-arms.
func (tc *timekeeperClock) WallTimeUntil(t, now ktime.Time) time.Duration {
	d := t.Sub(now)
	if f := tc.tk.dilationFloat; f > 1 {
		return time.Duration(float64(d) / f)
	}
	return d
}

// Now implements ktime.Clock.Now.
func (tc *timekeeperClock) Now() ktime.Time {
	now, err := tc.tk.GetTime(tc.c)
	if err != nil {
		panic(fmt.Sprintf("timekeeperClock(ClockID=%v)).Now: %v", tc.c, err))
	}
	return ktime.FromNanoseconds(now)
}

// NewTimer implements ktime.Clock.NewTimer.
func (tc *timekeeperClock) NewTimer(l ktime.Listener) ktime.Timer {
	return ktime.NewSampledTimer(tc, l)
}

var _ tcpip.Clock = (*Timekeeper)(nil)

// Now implements tcpip.Clock.
func (t *Timekeeper) Now() time.Time {
	nsec, err := t.GetTime(sentrytime.Realtime)
	if err != nil {
		panic("timekeeper.GetTime(sentrytime.Realtime): " + err.Error())
	}
	return time.Unix(0, nsec)
}

// NowMonotonic implements tcpip.Clock.
func (t *Timekeeper) NowMonotonic() tcpip.MonotonicTime {
	nsec, err := t.GetTime(sentrytime.Monotonic)
	if err != nil {
		panic("timekeeper.GetTime(sentrytime.Monotonic): " + err.Error())
	}
	var mt tcpip.MonotonicTime
	return mt.Add(time.Duration(nsec) * time.Nanosecond)
}

// AfterFunc implements tcpip.Clock.
func (t *Timekeeper) AfterFunc(d time.Duration, f func()) tcpip.Timer {
	timer := &timekeeperTcpipTimer{
		clock: t.monotonicClock,
		fn:    f,
	}
	timer.Reset(d)
	return timer
}

// timekeeperTcpipTimer implements tcpip.Timer by wrapping a ktime.SampledTimer.
// tcpip.Timer does not define a Destroy method, so each timer expiration and
// each call to Timer.Stop() must release all resources by calling
// ktime.SampledTimer.Destroy().
type timekeeperTcpipTimer struct {
	// immutable
	clock *timekeeperClock
	fn    func()

	// mu protects t.
	mu timekeeperTcpipTimerMutex

	// t stores the latest running Timer. This is replaced whenever Reset is
	// called since Timer cannot be restarted once it has been Destroyed by Stop.
	//
	// This field is nil iff Stop has been called.
	t *ktime.SampledTimer

	// resets is the number of times Reset has been called. resets is written
	// with both mu and ktime.SampledTimer locks held, so it may be read with
	// either or both locks held.
	resets int
}

// Stop implements tcpip.Timer.Stop.
func (r *timekeeperTcpipTimer) Stop() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.t == nil {
		return false
	}
	_, lastSetting := r.t.Set(ktime.Setting{}, nil)
	r.t.Destroy()
	r.t = nil
	return lastSetting.Enabled
}

// stopExpired is equivalent to Stop, but is called when the timer expires.
func (r *timekeeperTcpipTimer) stopExpired(reset int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.t == nil || r.resets != reset {
		return
	}
	r.t.Destroy()
	r.t = nil
}

// Reset implements tcpip.Timer.Reset.
func (r *timekeeperTcpipTimer) Reset(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.t == nil {
		r.t = ktime.NewSampledTimer(r.clock, r)
	}
	r.t.Set(ktime.Setting{
		Enabled: true,
		Next:    r.clock.Now().Add(d),
	}, r.incResets)
}

func (r *timekeeperTcpipTimer) incResets() {
	r.resets++
}

// NotifyTimer implements ktime.Listener.NotifyTimer.
func (r *timekeeperTcpipTimer) NotifyTimer(exp uint64) {
	// Implementations of ktime.Listener.NotifyTimer() can't call Timer methods
	// due to lock ordering, so we must call r.t.Destroy() from another
	// goroutine. We also must call r.stopExpired() rather than r.Stop(), since
	// the latter might cancel an unrelated call to r.Reset() that happens
	// between now and when this goroutine runs.
	thisReset := r.resets
	go func() {
		r.stopExpired(thisReset)
		r.fn()
	}()
}
