// Package collapse is a generic per-key singleflight whose leader runs on a
// context derived from a root, never from the caller: a waiter cancelling never
// kills the fetch the others wait on. It also reports waiter counts and offers
// TryStart for one-background-job-per-key dedupe.
package collapse

import (
	"context"
	"sync"
	"time"
)

// call is one in-flight leader and the waiters that joined it.
type call[T any] struct {
	done    chan struct{}
	val     T
	err     error
	waiters int // guarded by Group.mu
}

// Group collapses concurrent calls for the same key into one leader. The zero
// value is not usable; construct with New.
type Group[T any] struct {
	root context.Context //nolint:containedctx // the detached root every leader derives from, by design
	lead time.Duration

	mu    sync.Mutex
	calls map[string]*call[T]
	bg    map[string]struct{}
	// idle is closed when the group has nothing in flight; replaced with a fresh
	// channel on the transition from idle to busy. Wait selects on it.
	idle chan struct{}
}

// New returns a Group whose leaders run on a context derived from root with a
// lead budget (COLLAPSE_LEAD_TIMEOUT); the root cancelling aborts every leader.
// A nil root means context.Background(); a lead <= 0 means no timeout.
func New[T any](root context.Context, lead time.Duration) *Group[T] {
	if root == nil {
		root = context.Background()
	}
	idle := make(chan struct{})
	close(idle)
	return &Group[T]{
		root:  root,
		lead:  lead,
		calls: make(map[string]*call[T]),
		bg:    make(map[string]struct{}),
		idle:  idle,
	}
}

// Do runs fn once per key among concurrent callers. The leader's fn receives a
// context derived from the group's root (not ctx); ctx only decides how long
// this caller waits. It returns the value, whether it was shared with other
// callers, how many waiters joined the leader, and the leader's error (or ctx's
// error if this caller stopped waiting first).
//
// The leader runs fn inline on its own goroutine and always returns fn's result,
// even when its ctx is already cancelled: the leader's client vanishing must not
// abort the fetch the waiters depend on.
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, int, error) {
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.waiters++
		g.mu.Unlock()
		v, w, err := g.wait(ctx, c)
		return v, true, w, err
	}
	c := &call[T]{done: make(chan struct{})}
	g.calls[key] = c
	g.markBusyLocked()
	g.mu.Unlock()

	lctx, cancel := g.leadContext()
	func() {
		defer cancel()
		defer func() {
			// A panicking fn must not strand the waiters: publish the panic as
			// an error-free zero result and re-raise on the leader.
			if r := recover(); r != nil {
				g.finish(key, c)
				panic(r)
			}
		}()
		c.val, c.err = fn(lctx)
	}()
	waiters := g.finish(key, c)
	return c.val, waiters > 0, waiters, c.err
}

// wait blocks a joiner until the leader publishes or ctx ends; it returns the
// value, the waiter count and the leader's (or ctx's) error.
func (g *Group[T]) wait(ctx context.Context, c *call[T]) (T, int, error) {
	select {
	case <-c.done:
		g.mu.Lock()
		w := c.waiters
		g.mu.Unlock()
		return c.val, w, c.err
	case <-ctx.Done():
		var zero T
		g.mu.Lock()
		w := c.waiters
		g.mu.Unlock()
		return zero, w, ctx.Err()
	}
}

// finish publishes the call, removes it from the map and returns its waiter count.
func (g *Group[T]) finish(key string, c *call[T]) int {
	g.mu.Lock()
	if g.calls[key] == c {
		delete(g.calls, key)
	}
	waiters := c.waiters
	g.markIdleLocked()
	g.mu.Unlock()
	close(c.done)
	return waiters
}

// leadContext derives the leader's context from root with the lead budget.
func (g *Group[T]) leadContext() (context.Context, context.CancelFunc) {
	if g.lead > 0 {
		return context.WithTimeout(g.root, g.lead)
	}
	return context.WithCancel(g.root)
}

// TryStart claims key for a background job when nothing is running for it; ok
// is false while another job holds it. done releases the claim and must be
// called exactly once when ok (extra calls are ignored).
func (g *Group[T]) TryStart(key string) (done func(), ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, held := g.bg[key]; held {
		return func() {}, false
	}
	g.bg[key] = struct{}{}
	g.markBusyLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			delete(g.bg, key)
			g.markIdleLocked()
			g.mu.Unlock()
		})
	}, true
}

// Root returns the context every leader and background job derives from.
func (g *Group[T]) Root() context.Context { return g.root }

// InFlight reports how many keys currently have a leader or a background claim.
func (g *Group[T]) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlightLocked()
}

// Wait blocks until nothing is in flight (no leader, no background claim) or
// ctx ends. New work arriving while waiting extends the wait.
func (g *Group[T]) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.inFlightLocked() == 0 {
			g.mu.Unlock()
			return nil
		}
		idle := g.idle
		g.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *Group[T]) inFlightLocked() int {
	n := len(g.calls)
	for k := range g.bg {
		if _, dup := g.calls[k]; !dup {
			n++
		}
	}
	return n
}

// markBusyLocked opens a fresh idle channel on the idle -> busy transition.
func (g *Group[T]) markBusyLocked() {
	select {
	case <-g.idle:
		g.idle = make(chan struct{})
	default:
	}
}

// markIdleLocked closes the idle channel once nothing is left in flight.
func (g *Group[T]) markIdleLocked() {
	if g.inFlightLocked() != 0 {
		return
	}
	select {
	case <-g.idle:
	default:
		close(g.idle)
	}
}
