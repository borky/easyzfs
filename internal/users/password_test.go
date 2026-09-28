package users

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of password checks runs one argon2 computation at a time: each
// allocates 64 MiB, and four at once got the service OOM-killed under its
// 256 MB cap.
func TestArgonIsSerialised(t *testing.T) {
	phc, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	var running, peak atomic.Int32
	saved := idKey
	t.Cleanup(func() { idKey = saved })
	idKey = func(pw, salt []byte, tm, mem uint32, th uint8, kl uint32) []byte {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		defer running.Add(-1)
		return saved(pw, salt, tm, mem, th, kl)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !verifyPassword("correct horse", phc) {
				t.Error("verify failed")
			}
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d argon2 computations ran at once, want 1", peak.Load())
	}
}

// A stored hash claiming a huge memory cost (say, from an imported backup)
// is refused without allocating it.
func TestVerifyRefusesHostileParameters(t *testing.T) {
	for _, phc := range []string{
		"$argon2id$v=19$m=4194304,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=65536,t=100000,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g",
	} {
		if verifyPassword("x", phc) {
			t.Errorf("accepted %s", phc)
		}
	}
}
