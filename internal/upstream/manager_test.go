package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/shabilullah/wakil-mcp/internal/config"
)

// unreachableManager builds a manager with one eager upstream whose command
// does not exist, so every connect attempt fails immediately.
func unreachableManager(t *testing.T, initial time.Duration) *Manager {
	t.Helper()

	m := NewManager(Options{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReapInterval: time.Hour,
	})
	t.Cleanup(func() { _ = m.Close() })

	m.SetServers(map[string]Def{
		"broken": {
			Key: "broken",
			Cfg: &config.Upstream{
				Command:     "definitely-not-a-real-program-xyz",
				StartPolicy: config.StartEager,
			},
			Defaults: config.ServerDefaults{
				RestartBackoff: config.Backoff{
					Initial: config.Duration(initial),
					Max:     config.Duration(time.Minute),
					Factor:  2,
				},
			},
		},
	})
	return m
}

// failedOnce asserts that the upstream has failed and returns its retry time.
func failedOnce(t *testing.T, m *Manager) time.Time {
	t.Helper()

	m.mu.Lock()
	e := m.entries["broken"]
	m.mu.Unlock()
	if e == nil {
		t.Fatal("the upstream vanished from the manager")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failures == 0 {
		t.Fatal("the attempt did not fail, so there is no retry to wait for")
	}
	return e.nextAttempt
}

// TestEnumerateDoesNotWaitOutABackoff covers the reload path.
//
// A config save re-enumerates the upstreams, and a server already waiting to
// retry has nothing to contribute: sitting on its timer would stall whoever
// triggered the reload, which is every client edit made through the WebUI.
func TestEnumerateDoesNotWaitOutABackoff(t *testing.T) {
	m := unreachableManager(t, 30*time.Second)
	ctx := context.Background()

	m.Enumerate(ctx)
	if next := failedOnce(t, m); !next.After(time.Now()) {
		t.Fatalf("expected a scheduled retry, got %s", next)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Enumerate(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enumerate waited for the retry timer instead of skipping the upstream")
	}
}

// TestAcquireStillWaitsForRetry is the other half of the same decision: a caller
// that needs the connection waits for the retry instead of being turned away.
func TestAcquireStillWaitsForRetry(t *testing.T) {
	m := unreachableManager(t, 30*time.Second)
	m.Enumerate(context.Background())
	failedOnce(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := m.Acquire(ctx, "broken")
	if errors.Is(err, ErrBackoff) {
		t.Fatal("a caller acting on a demand was told to come back later instead of waiting")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the wait to end with the context, got %v", err)
	}
}
