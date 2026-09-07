package truenas

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/pwatteel/truenas-csi/internal/truenas/fake"
)

// TestConcurrencyCapUnderLoad proves the client never exceeds the appliance's
// in-flight ceiling. Measured on the real box: 20 concurrent calls succeed and
// everything beyond fails with -32000, so the client caps itself at 16.
func TestConcurrencyCapUnderLoad(t *testing.T) {
	s := fake.Start(t, fake.Options{ConcurrencyLimit: 20})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		time.Sleep(5 * time.Millisecond) // hold the slot so concurrency is observable
		return []any{}, nil
	})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Call(ctx, "pool.dataset.query"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("call failed under load: %v", err)
	}
	if peak := s.PeakConcurrency(); peak > MaxInFlight {
		t.Fatalf("peak in-flight %d exceeds MaxInFlight %d — the semaphore is not holding", peak, MaxInFlight)
	}
	if s.PeakConcurrency() < 2 {
		t.Fatalf("peak in-flight %d — the load never actually overlapped, test proves nothing", s.PeakConcurrency())
	}
}

func TestConnectionFailureRetries(t *testing.T) {
	s := fake.Start(t, fake.Options{DropAfter: 1})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// The fake drops the connection after that call; the next must reconnect.
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("call after connection drop should reconnect and succeed, got %v", err)
	}
	if got := s.AuthCount(); got < 2 {
		t.Fatalf("auth count %d — a reconnect must re-authenticate", got)
	}
}

func TestNotificationsAreRouted(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s.Notify("collection_update", map[string]any{
		"collection": "core.get_jobs", "id": 9615,
		"fields": map[string]any{"state": "RUNNING"},
	})

	select {
	case n := <-c.Notifications():
		if n.Method != "collection_update" {
			t.Fatalf("got notification method %q", n.Method)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification never arrived on Notifications()")
	}

	// An id-less message must not disturb ordinary calls.
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("call after notification: %v", err)
	}
}

func TestTooManyConcurrentIsTypedError(t *testing.T) {
	s := fake.Start(t, fake.Options{ConcurrencyLimit: 0})
	s.Handle("boom", func([]json.RawMessage) (any, error) {
		return nil, &fake.RPCError{Code: -32000, Reason: "too many concurrent calls"}
	})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Call(context.Background(), "boom")
	if err == nil || !isTooMany(err) {
		t.Fatalf("want ErrTooManyConcurrent, got %v", err)
	}
}

func isTooMany(err error) bool {
	for e := err; e != nil; {
		if e == ErrTooManyConcurrent {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
