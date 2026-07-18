package executor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Behavior: a registered server's executor is returned immediately; an
// unknown server fails with ErrUnreachable after the window; an executor
// registering during the window unblocks the waiter.
func TestRegistryAcquire(t *testing.T) {
	r := NewRegistry(80 * time.Millisecond)
	local := &Local{}
	r.Register("s1", local)

	got, err := r.Acquire(context.Background(), "s1")
	if err != nil || got != Executor(local) {
		t.Fatalf("acquire local: %v %v", got, err)
	}

	start := time.Now()
	_, err = r.Acquire(context.Background(), "ghost")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatal("acquire returned before the window elapsed")
	}

	// late registration within the window
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.Register("late", local)
	}()
	if _, err := r.Acquire(context.Background(), "late"); err != nil {
		t.Fatalf("late registration should unblock acquire: %v", err)
	}
}
