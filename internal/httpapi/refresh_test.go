// refresh_test.go — a successful storage mutation refreshes the pool
// collector at once; failures, reads and non-storage routes do not.
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"easyzfs/internal/model"
)

type countingPools struct{ refreshes int }

func (c *countingPools) Pools() []model.Pool                 { return nil }
func (c *countingPools) Datasets() []model.Dataset           { return nil }
func (c *countingPools) SnapshotGroups() []model.SnapGroup   { return nil }
func (c *countingPools) History(string) []model.HistoryEntry { return nil }
func (c *countingPools) RefreshSoon()                        { c.refreshes++ }

func TestRefreshAfterMutation(t *testing.T) {
	for _, c := range []struct {
		method, path string
		status       int
		want         int
	}{
		{"POST", "/api/pools/tank/scrub", http.StatusAccepted, 1},
		{"DELETE", "/api/datasets/tank%2Fx", http.StatusAccepted, 1},
		{"POST", "/api/pools/tank/scrub", http.StatusConflict, 0}, // failed: nothing changed
		{"GET", "/api/pools", http.StatusOK, 0},
		{"PUT", "/api/settings", http.StatusNoContent, 0}, // not storage
	} {
		pools := &countingPools{}
		s := &Server{pools: pools}
		h := s.refreshAfterMutation(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if pools.refreshes != c.want {
			t.Errorf("%s %s -> %d: %d refreshes, want %d", c.method, c.path, c.status, pools.refreshes, c.want)
		}
	}
}
