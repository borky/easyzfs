// hoststorage.go — which pools and datasets belong to the host itself (the
// running OS, Proxmox storage and guest disks), for the views: they show it
// and leave out what would be refused. The actions do not use this cache;
// they read the host again before acting (actions.guardHost).
package collectors

import (
	"context"
	"log"
	"sync"
	"time"

	"easyzfs/internal/actions"
)

// HostStorageProvider — the last view of the host; nil before the first read
// or after a failed one (the views then show nothing as protected, and the
// actions, which re-read, refuse).
type HostStorageProvider interface {
	HostView() *actions.HostView
}

type HostStorageCollector struct {
	mu        sync.Mutex
	view      *actions.HostView
	interval  time.Duration
	refreshCh chan struct{}
	read      func(context.Context) (*actions.HostView, error)
}

func NewHostStorageCollector() *HostStorageCollector {
	return &HostStorageCollector{
		interval:  time.Minute,
		refreshCh: make(chan struct{}, 1),
		read:      actions.LoadHostView,
	}
}

func (c *HostStorageCollector) Name() string { return "hoststorage" }

func (c *HostStorageCollector) HostView() *actions.HostView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.view
}

// RefreshSoon — asks for a read now (after a mutation; coalesces).
func (c *HostStorageCollector) RefreshSoon() {
	select {
	case c.refreshCh <- struct{}{}:
	default:
	}
}

func (c *HostStorageCollector) collect(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, err := c.read(cctx)
	if err != nil {
		log.Printf("hoststorage: %v", err)
	}
	c.mu.Lock()
	c.view = v
	c.mu.Unlock()
}

func (c *HostStorageCollector) Run(ctx context.Context) {
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

// noHostStorage — mock deployments have no host to read.
type noHostStorage struct{}

func (noHostStorage) HostView() *actions.HostView { return nil }
