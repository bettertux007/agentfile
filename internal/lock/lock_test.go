package lock

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAcquireReleaseAndReuse(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "nested", "test.lock")

	l1, err := Acquire(lockPath)
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should not be unlinked on close: %v", err)
	}

	// Re-acquiring on the same path works cleanly.
	l2, err := Acquire(lockPath)
	if err != nil {
		t.Fatalf("second Acquire failed: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
}

func TestAcquireMutualExclusion(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "concurrent.lock")

	const workers = 6
	var active atomic.Int32
	var maxObserved atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			l, err := Acquire(lockPath)
			if err != nil {
				t.Errorf("Acquire failed: %v", err)
				return
			}
			cur := active.Add(1)
			for {
				prev := maxObserved.Load()
				if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
					break
				}
			}
			active.Add(-1)
			if err := l.Close(); err != nil {
				t.Errorf("Close failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if maxObserved.Load() != 1 {
		t.Fatalf("expected mutual exclusion (max active 1), observed %d", maxObserved.Load())
	}
}
