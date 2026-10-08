package collapse

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoCollapsesAndSurvivesLeaderCancel(t *testing.T) {
	t.Parallel()
	g := New[string](context.Background(), 5*time.Second)

	var fetches atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func(ctx context.Context) (string, error) {
		fetches.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "value", nil
	}

	const n = 100
	type result struct {
		val    string
		shared bool
		err    error
	}
	results := make([]result, n)
	var wg sync.WaitGroup

	// The first caller becomes the leader; its own context is cancelled at once.
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	wg.Add(1)
	go func() {
		defer wg.Done()
		v, shared, _, err := g.Do(leaderCtx, "k", fn)
		results[0] = result{v, shared, err}
	}()
	<-started
	cancelLeader()

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, shared, _, err := g.Do(context.Background(), "k", fn)
			results[i] = result{v, shared, err}
		}()
	}
	// Let the waiters join before the leader is released.
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		w := 0
		if c := g.calls["k"]; c != nil {
			w = c.waiters
		}
		g.mu.Unlock()
		if w == n-1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	for i, r := range results {
		if r.err != nil || r.val != "value" {
			t.Fatalf("caller %d: val=%q err=%v", i, r.val, r.err)
		}
		if !r.shared {
			t.Fatalf("caller %d: shared=false, want true", i)
		}
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight = %d after completion", g.InFlight())
	}
}

func TestDoWaiterHonoursOwnContext(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _, _, _ = g.Do(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 1, nil
		})
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, shared, _, err := g.Do(ctx, "k", func(context.Context) (int, error) { return 2, nil })
	if !errors.Is(err, context.Canceled) || !shared {
		t.Fatalf("waiter: shared=%v err=%v, want shared + Canceled", shared, err)
	}
	close(release)
}

func TestDoLeaderTimeoutReachesEveryone(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), 20*time.Millisecond)
	fn := func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	const n = 10
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, errs[i] = g.Do(context.Background(), "slow", fn)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller %d: err=%v, want DeadlineExceeded", i, err)
		}
	}
}

func TestDoRootCancelAbortsLeader(t *testing.T) {
	t.Parallel()
	root, cancel := context.WithCancel(context.Background())
	g := New[int](root, time.Minute)
	started := make(chan struct{})
	go func() {
		<-started
		cancel()
	}()
	_, _, _, err := g.Do(context.Background(), "k", func(ctx context.Context) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want Canceled from root", err)
	}
}

func TestDoDistinctKeysRunConcurrently(t *testing.T) {
	t.Parallel()
	g := New[string](context.Background(), time.Second)
	var wg sync.WaitGroup
	var both sync.WaitGroup
	both.Add(2)
	for _, k := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, shared, waiters, err := g.Do(context.Background(), k, func(context.Context) (string, error) {
				both.Done()
				both.Wait() // deadlocks unless both leaders run at once
				return k, nil
			})
			if err != nil || v != k || shared || waiters != 0 {
				t.Errorf("key %s: v=%q shared=%v waiters=%d err=%v", k, v, shared, waiters, err)
			}
		}()
	}
	wg.Wait()
}

func TestDoErrorPropagates(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	want := errors.New("boom")
	_, _, _, err := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 0, want })
	if !errors.Is(err, want) {
		t.Fatalf("err=%v, want %v", err, want)
	}
	// The key is free again afterwards.
	v, _, _, err := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("second Do: v=%d err=%v", v, err)
	}
}

func TestTryStartDedupes(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	done, ok := g.TryStart("k")
	if !ok {
		t.Fatal("first TryStart should succeed")
	}
	if g.InFlight() != 1 {
		t.Fatalf("InFlight = %d, want 1", g.InFlight())
	}
	if _, again := g.TryStart("k"); again {
		t.Fatal("second TryStart should be refused while the first holds the key")
	}
	if _, other := g.TryStart("other"); !other {
		t.Fatal("a different key must not be blocked")
	}
	done()
	done() // idempotent
	if _, ok := g.TryStart("k"); !ok {
		t.Fatal("TryStart should succeed after done")
	}
}

func TestTryStartConcurrentOneWinner(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if done, ok := g.TryStart("k"); ok {
				wins.Add(1)
				time.Sleep(5 * time.Millisecond)
				done()
			}
		}()
	}
	wg.Wait()
	if wins.Load() < 1 {
		t.Fatal("nobody won TryStart")
	}
}

func TestWait(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait on idle group: %v", err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_, _, _, _ = g.Do(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			return 0, nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with work in flight: err=%v, want DeadlineExceeded", err)
	}
	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after completion: %v", err)
	}
	if g.InFlight() != 0 {
		t.Fatalf("InFlight = %d", g.InFlight())
	}
}

func TestDoPanicReleasesWaiters(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		<-started
		go func() {
			_, _, _, err := g.Do(context.Background(), "k", func(context.Context) (int, error) { return 1, nil })
			waiterDone <- err
		}()
		// Release the leader only once the waiter has joined.
		for {
			g.mu.Lock()
			c := g.calls["k"]
			joined := c != nil && c.waiters == 1
			g.mu.Unlock()
			if joined {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("leader panic was swallowed")
			}
		}()
		_, _, _, _ = g.Do(context.Background(), "k", func(context.Context) (int, error) {
			close(started)
			<-release
			panic("leader exploded")
		})
	}()
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never released after leader panic")
	}
}

func TestDoEarlyReleasesWaitersOnPublish(t *testing.T) {
	t.Parallel()
	g := New[string](context.Background(), 5*time.Second)

	published := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fn := func(ctx context.Context, publish func(string)) (string, error) {
		calls.Add(1)
		publish("early")
		publish("ignored") // only the first publish counts
		close(published)
		<-release
		return "final", ctx.Err()
	}

	leader := make(chan string, 1)
	go func() {
		v, _, early, _, err := g.DoEarly(context.Background(), "k", fn)
		if err != nil || early {
			t.Errorf("leader: early=%v err=%v", early, err)
		}
		leader <- v
	}()
	<-published

	// A waiter joining after the publish returns the early value at once,
	// while the leader is still running.
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, shared, early, _, err := g.DoEarly(context.Background(), "k", fn)
			if v != "early" || !shared || !early || err != nil {
				t.Errorf("waiter: v=%q shared=%v early=%v err=%v", v, shared, early, err)
			}
		}()
	}
	wg.Wait()
	select {
	case v := <-leader:
		t.Fatalf("leader finished before release: %q", v)
	default:
	}
	close(release)
	if v := <-leader; v != "final" {
		t.Errorf("leader value = %q, want final", v)
	}
	if c := calls.Load(); c != 1 {
		t.Errorf("fn calls = %d, want 1", c)
	}
}

func TestDoEarlyBlockedWaiterWakesOnPublish(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), 5*time.Second)

	started := make(chan struct{})
	doPublish := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _, _, _, _ = g.DoEarly(context.Background(), "k", func(_ context.Context, publish func(int)) (int, error) {
			close(started)
			<-doPublish
			publish(1)
			<-release
			return 2, nil
		})
	}()
	<-started

	got := make(chan bool, 1)
	go func() {
		v, _, early, _, _ := g.DoEarly(context.Background(), "k", func(context.Context, func(int)) (int, error) {
			t.Error("waiter ran fn")
			return 0, nil
		})
		got <- early && v == 1
	}()
	waitFor(t, func() bool { return g.waiterCountFor("k") == 1 })
	close(doPublish)
	if ok := <-got; !ok {
		t.Error("blocked waiter did not get the early value")
	}
	close(release)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDoEarlyWithoutPublishWaitsForFinal(t *testing.T) {
	t.Parallel()
	g := New[int](context.Background(), 5*time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _, _, _, _ = g.DoEarly(context.Background(), "k", func(context.Context, func(int)) (int, error) {
			close(started)
			<-release
			return 7, nil
		})
	}()
	<-started
	got := make(chan int, 1)
	go func() {
		v, _, early, _, _ := g.DoEarly(context.Background(), "k", nil)
		if early {
			t.Error("early without a publish")
		}
		got <- v
	}()
	waitFor(t, func() bool { return g.waiterCountFor("k") == 1 })
	close(release)
	if v := <-got; v != 7 {
		t.Errorf("waiter value = %d, want 7", v)
	}
}

// waiterCountFor reports the waiters joined to key's in-flight call (0 if none).
func (g *Group[T]) waiterCountFor(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return c.waiters
	}
	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}
