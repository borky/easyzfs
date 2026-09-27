// diskuse.go — why each disk outside a pool is not free (mounted, swap, LVM,
// an ESP, a ZFS label…), for the disk view. It used to be read by the
// /api/disks handler itself on a cache miss, which broke the rule that
// handlers only read caches: 1+N lsblk runs under a lock every view.
// The actions do not use this: they re-check each disk live before acting.
package collectors

import (
	"context"
	"log"
	"sync"
	"time"

	"easyzfs/internal/actions"
)

// DiskUseProvider — kernel name → reason; a disk absent from the map is free.
type DiskUseProvider interface {
	DiskUse() map[string]string
}

// DiskUseCollector refreshes the map on a ticker, and soon after a storage
// mutation (RefreshSoon).
type DiskUseCollector struct {
	mu        sync.Mutex
	m         map[string]string
	interval  time.Duration
	refreshCh chan struct{}
	read      func(context.Context) (map[string]string, error)
}

func NewDiskUseCollector() *DiskUseCollector {
	return &DiskUseCollector{
		interval:  30 * time.Second,
		refreshCh: make(chan struct{}, 1),
		read:      actions.AllDiskUse,
	}
}

func (c *DiskUseCollector) Name() string { return "diskuse" }

// DiskUse — the last map read; nil before the first read or after a failed
// one, which the view shows as "no reason known", as before.
func (c *DiskUseCollector) DiskUse() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m
}

// RefreshSoon — asks for a read now (non-blocking; coalesces).
func (c *DiskUseCollector) RefreshSoon() {
	select {
	case c.refreshCh <- struct{}{}:
	default:
	}
}

func (c *DiskUseCollector) collect(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := c.read(cctx)
	if err != nil {
		log.Printf("diskuse: %v", err)
		m = nil // stale reasons would be worse than none: the actions re-check anyway
	}
	c.mu.Lock()
	c.m = m
	c.mu.Unlock()
}

func (c *DiskUseCollector) Run(ctx context.Context) {
	c.collect(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.refreshCh:
		}
		c.collect(ctx)
	}
}

// noDiskUse — mock deployments have no real disks to classify.
type noDiskUse struct{}

func (noDiskUse) DiskUse() map[string]string { return nil }
