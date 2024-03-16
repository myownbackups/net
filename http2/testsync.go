// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package http2

import (
	"sync"
	"time"
)

// testSyncHooks coordinates goroutines in tests.
//
// For example, a call to ClientConn.RoundTrip involves several goroutines, including:
//   - the goroutine running RoundTrip;
//   - the clientStream.doRequest goroutine, which writes the request; and
//   - the clientStream.readLoop goroutine, which reads the response.
//
// Using testSyncHooks, a test can start a RoundTrip and identify when all these goroutines
// are blocked waiting for some condition such as reading the Request.Body or waiting for
// flow control to become available.
//
// The testSyncHooks also manage timers and synthetic time in tests.
// This permits us to, for example, start a request and cause it to time out waiting for
// response headers without resorting to time.Sleep calls.
type testSyncHooks struct {
	// active/inactive act as a mutex and condition variable.
	//
	//  - neither chan contains a value: testSyncHooks is locked.
	//  - active contains a value: unlocked, and at least one goroutine is not blocked
	//  - inactive contains a value: unlocked, and all goroutines are blocked
	active   chan struct{}
	inactive chan struct{}

	// goroutine counts
	total    int                     // total goroutines
	condwait map[*sync.Cond]int      // blocked in sync.Cond.Wait
	blocked  []*testBlockedGoroutine // otherwise blocked

	// fake time
	now    time.Time
	timers []*fakeTimer

	// Transport testing: Report various events.
	newclientconn func(*ClientConn)
	newstream     func(*clientStream)
}

// testBlockedGoroutine is a blocked goroutine.
type testBlockedGoroutine struct {
	f  func() bool   // blocked until f returns true
	ch chan struct{} // closed when unblocked
}

// func newTestSyncHooks() *testSyncHooks {
// 	h := &testSyncHooks{
// 		active:   make(chan struct{}, 1),
// 		inactive: make(chan struct{}, 1),
// 		condwait: map[*sync.Cond]int{},
// 	}
// 	h.inactive <- struct{}{}
// 	h.now = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
// 	return h
// }

// lock acquires the testSyncHooks mutex.
func (h *testSyncHooks) lock() {
	select {
	case <-h.active:
	case <-h.inactive:
	}
}

// waitInactive waits for all goroutines to become inactive.
// func (h *testSyncHooks) waitInactive() {
// 	for {
// 		<-h.inactive
// 		if !h.unlock() {
// 			break
// 		}
// 	}
// }

// unlock releases the testSyncHooks mutex.
// It reports whether any goroutines are active.
func (h *testSyncHooks) unlock() (active bool) {
	// Look for a blocked goroutine which can be unblocked.
	blocked := h.blocked[:0]
	unblocked := false
	for _, b := range h.blocked {
		if !unblocked && b.f() {
			unblocked = true
			close(b.ch)
		} else {
			blocked = append(blocked, b)
		}
	}
	h.blocked = blocked

	// Count goroutines blocked on condition variables.
	condwait := 0
	for _, count := range h.condwait {
		condwait += count
	}

	if h.total > condwait+len(blocked) {
		h.active <- struct{}{}
		return true
	} else {
		h.inactive <- struct{}{}
		return false
	}
}

// goRun starts a new goroutine.
func (h *testSyncHooks) goRun(f func()) {
	h.lock()
	h.total++
	h.unlock()
	go func() {
		defer func() {
			h.lock()
			h.total--
			h.unlock()
		}()
		f()
	}()
}

// blockUntil indicates that a goroutine is blocked waiting for some condition to become true.
// It waits until f returns true before proceeding.
//
// Example usage:
//
//	h.blockUntil(func() bool {
//		// Is the context done yet?
//		select {
//		case <-ctx.Done():
//		default:
//			return false
//		}
//		return true
//	})
//	// Wait for the context to become done.
//	<-ctx.Done()
//
// The function f passed to blockUntil must be non-blocking and idempotent.
func (h *testSyncHooks) blockUntil(f func() bool) {
	if f() {
		return
	}
	ch := make(chan struct{})
	h.lock()
	h.blocked = append(h.blocked, &testBlockedGoroutine{
		f:  f,
		ch: ch,
	})
	h.unlock()
	<-ch
}

// broadcast is sync.Cond.Broadcast.
func (h *testSyncHooks) condBroadcast(cond *sync.Cond) {
	h.lock()
	delete(h.condwait, cond)
	h.unlock()
	cond.Broadcast()
}

// broadcast is sync.Cond.Wait.
func (h *testSyncHooks) condWait(cond *sync.Cond) {
	h.lock()
	h.condwait[cond]++
	h.unlock()
}

// newTimer creates a new timer: A time.Timer if h is nil, or a synthetic timer in tests.
func (h *testSyncHooks) newTimer(d time.Duration) timer {
	h.lock()
	defer h.unlock()
	t := &fakeTimer{
		when: h.now.Add(d),
		c:    make(chan time.Time),
	}
	h.timers = append(h.timers, t)
	return t
}

// advance advances time and causes synthetic timers to fire.
// func (h *testSyncHooks) advance(d time.Duration) {
// 	h.lock()
// 	defer h.unlock()
// 	h.now = h.now.Add(d)
// 	timers := h.timers[:0]
// 	for _, t := range h.timers {
// 		t.mu.Lock()
// 		switch {
// 		case t.when.After(h.now):
// 			timers = append(timers, t)
// 		case t.when.IsZero():
// 			// stopped timer
// 		default:
// 			t.when = time.Time{}
// 			close(t.c)
// 		}
// 		t.mu.Unlock()
// 	}
// 	h.timers = timers
// }

// A timer wraps a time.Timer, or a synthetic equivalent in tests.
// Unlike time.Timer, timer is single-use: The timer channel is closed when the timer expires.
type timer interface {
	C() <-chan time.Time
	Stop() bool
}

type timeTimer struct {
	t *time.Timer
	c chan time.Time
}

func newTimeTimer(d time.Duration) timer {
	ch := make(chan time.Time)
	t := time.AfterFunc(d, func() {
		close(ch)
	})
	return &timeTimer{t, ch}
}

func (t timeTimer) C() <-chan time.Time { return t.c }
func (t timeTimer) Stop() bool          { return t.t.Stop() }

type fakeTimer struct {
	mu   sync.Mutex
	when time.Time
	c    chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	stopped := t.when.IsZero()
	t.when = time.Time{}
	return stopped
}
