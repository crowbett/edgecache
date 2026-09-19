// Package collapse is a generic per-key singleflight whose leader runs on a
// context derived from a root, never from the caller: a waiter cancelling never
// kills the fetch the others wait on. It also reports waiter counts and offers
// TryStart for one-background-job-per-key dedupe.
//
// STUB written by the integrator to freeze the contract; workstream C owns this
// package and replaces the bodies, keeping every exported signature.
package collapse

import (
	"context"
	"errors"
	"time"
)

// Group collapses concurrent calls for the same key into one leader. The zero
// value is not usable; construct with New.
type Group[T any] struct {
	root context.Context //nolint:containedctx // the detached root every leader derives from, by design
	lead time.Duration
}

// New returns a Group whose leaders run on a context derived from root with a
// lead budget (COLLAPSE_LEAD_TIMEOUT); the root cancelling aborts every leader.
func New[T any](root context.Context, lead time.Duration) *Group[T] {
	return &Group[T]{root: root, lead: lead}
}

// Do runs fn once per key among concurrent callers. The leader's fn receives a
// context derived from the group's root (not ctx); ctx only decides how long
// this caller waits. It returns the value, whether it was shared with other
// callers, how many waiters joined the leader, and the leader's error (or ctx's
// error if this caller stopped waiting first).
func (g *Group[T]) Do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, bool, int, error) {
	var zero T
	_, _, _, _ = g, ctx, key, fn
	return zero, false, 0, errors.New("stub: collapse.Group.Do")
}

// TryStart claims key for a background job when nothing is running for it; ok
// is false while another job holds it. done releases the claim and must be
// called exactly once when ok.
func (g *Group[T]) TryStart(key string) (done func(), ok bool) {
	_, _ = g, key
	return func() {}, false
}

// InFlight reports how many keys currently have a leader or a background claim.
func (g *Group[T]) InFlight() int {
	_ = g
	return 0
}
