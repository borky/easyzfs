package collectors

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The view reads a cache; a mutation asks for a fresh read; a failed read
// clears the reasons rather than keeping stale ones.
func TestDiskUseCollectorRefreshAndFailure(t *testing.T) {
	var calls atomic.Int32
	c := NewDiskUseCollector()
	c.interval = time.Hour
	c.read = func(context.Context) (map[string]string, error) {
		if calls.Add(1) >= 3 {
			return nil, errors.New("lsblk failed")
		}
		return map[string]string{"sdb": "sdb es un volumen físico LVM"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor := func(n int32) {
		t.Helper()
		for deadline := time.Now().Add(2 * time.Second); calls.Load() < n; {
			if time.Now().After(deadline) {
				t.Fatalf("only %d reads, want %d", calls.Load(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(1)
	if c.DiskUse()["sdb"] == "" {
		t.Fatal("first read not cached")
	}
	c.RefreshSoon()
	waitFor(2)
	c.RefreshSoon()
	waitFor(3)
	if c.DiskUse() != nil {
		t.Fatal("a failed read kept stale reasons")
	}
}
