// longops_test.go — ciclo de vida del runner: proceso corto que completa,
// proceso cancelado, salida capturada, TTL y RunningFor.
package longops

import (
	"syscall"
	"testing"
	"time"

	"easyzfs/internal/executil"
	"easyzfs/internal/hub"
)

func init() {
	// Los procesos de prueba (sleep/printf/true/false) no necesitan sudo.
	executil.SetSudoForTest(false)
}

// waitStatus espera (máx. 3 s) a que la op alcance un estado terminal.
func waitStatus(t *testing.T, m *Manager, id, want string) Op {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, op := range m.List() {
			if op.ID == id && op.Status == want {
				return op
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, op := range m.List() {
		if op.ID == id {
			t.Fatalf("op %s quedó en %q, esperaba %q", id, op.Status, want)
		}
	}
	t.Fatalf("op %s desapareció antes de alcanzar %q", id, want)
	return Op{}
}

func TestStartCompletes(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.Start("rewrite", "tank/docs", "sleep", "0.2")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op.Status != StatusRunning || op.PID <= 0 {
		t.Fatalf("op inicial: status=%q pid=%d", op.Status, op.PID)
	}
	if !m.RunningFor("tank/docs") {
		t.Error("RunningFor(tank/docs)=false con op en curso")
	}
	fin := waitStatus(t, m, op.ID, StatusDone)
	if fin.Ended == nil {
		t.Error("Ended nil tras completar")
	}
	if m.RunningFor("tank/docs") {
		t.Error("RunningFor=true tras completar")
	}
}

func TestStartCapturesOutput(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.Start("rewrite", "tank/docs", "printf", "linea1\nlinea2\n")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	fin := waitStatus(t, m, op.ID, StatusDone)
	if len(fin.Lines) != 2 || fin.Lines[0] != "linea1" || fin.Lines[1] != "linea2" {
		t.Errorf("lines=%v, esperaba [linea1 linea2]", fin.Lines)
	}
}

func TestCancel(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.Start("rewrite", "tank/docs", "sleep", "30")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Cancel(op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitStatus(t, m, op.ID, StatusCanceled)
	// Cancelar de nuevo: ya no está en curso.
	if err := m.Cancel(op.ID); err != ErrNotRunning {
		t.Errorf("re-cancel err=%v, esperaba ErrNotRunning", err)
	}
	if err := m.Cancel("op-inexistente"); err != ErrNotFound {
		t.Errorf("cancel inexistente err=%v, esperaba ErrNotFound", err)
	}
}

func TestErrorStatus(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.Start("rewrite", "tank/docs", "false")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	fin := waitStatus(t, m, op.ID, StatusError)
	if fin.Error == "" {
		t.Error("Error vacío en op fallida")
	}
}

func TestListPurgesOld(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.Start("rewrite", "tank/docs", "true")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, op.ID, StatusDone)
	// Envejece la entrada artificialmente más allá del TTL.
	m.mu.Lock()
	past := time.Now().Add(-2 * doneTTL)
	m.ops[0].Ended = &past
	m.mu.Unlock()
	if got := len(m.List()); got != 0 {
		t.Errorf("List=%d entradas, esperaba 0 (purgada por TTL)", got)
	}
}

// StartPipeline: stdout of one stage feeds the next with no shell between.
func TestPipelineConnectsStages(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.StartPipeline("replication", "tank/a",
		Stage{Name: "printf", Args: []string{"alpha\nbeta\n"}},
		Stage{Name: "tr", Args: []string{"a-z", "A-Z"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	fin := waitStatus(t, m, op.ID, StatusDone)
	if len(fin.Lines) != 2 || fin.Lines[0] != "ALPHA" || fin.Lines[1] != "BETA" {
		t.Errorf("lines=%v, want [ALPHA BETA]", fin.Lines)
	}
}

// Like 'set -o pipefail': a failing first stage fails the operation even
// though the last one exits 0.
func TestPipelineFailsWhenAnyStageFails(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.StartPipeline("replication", "tank/a",
		Stage{Name: "false"},
		Stage{Name: "cat"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitStatus(t, m, op.ID, StatusError)
}

// Cancel must kill every stage, not only the first.
func TestPipelineCancelKillsAllStages(t *testing.T) {
	m := New(hub.NewHub())
	op, err := m.StartPipeline("replication", "tank/a",
		Stage{Name: "sleep", Args: []string{"30"}},
		Stage{Name: "sleep", Args: []string{"31"}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := m.Cancel(op.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitStatus(t, m, op.ID, StatusCanceled)
	for _, pid := range op.pids {
		if syscall.Kill(pid, 0) == nil {
			t.Errorf("stage pid %d still alive after cancel", pid)
		}
	}
}
