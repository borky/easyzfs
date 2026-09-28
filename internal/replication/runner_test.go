package replication

import (
	"context"
	"errors"
	"testing"

	"easyzfs/internal/actions"
)

// Every sudo'd stage the runner builds is a shape the privileged gateway
// accepts: it refuses anything else at runtime, which the other tests (run
// without the gateway) would never see. Its host checks may still refuse
// here, reading this machine; only "not a known shape" fails.
func TestStagesPassTheGatewayGrammar(t *testing.T) {
	r := &Runner{dataDir: t.TempDir()}
	for _, j := range []*Job{localJob(), sshJob()} {
		for _, incremental := range []bool{false, true} {
			for _, st := range r.stages(j, j.Source+"@ezrepl-x", incremental) {
				if !st.Sudo {
					continue
				}
				if err := actions.PrivCheck(context.Background(), st.Name, st.Args); errors.Is(err, actions.ErrNotAllowed) {
					t.Errorf("%s: gateway refuses %s %v", j.DestType, st.Name, st.Args)
				}
			}
		}
	}
}
