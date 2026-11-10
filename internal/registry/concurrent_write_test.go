package registry

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWritesDoNotFail reproduces the daemon's write pattern: the
// lifecycle manager, the API server and the router callbacks all write the
// registry from different goroutines. No write may fail with SQLITE_BUSY,
// because a dropped write loses state such as a saved public port.
func TestConcurrentWritesDoNotFail(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ovm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const writers = 16
	const writesEach = 40

	for i := 0; i < writers; i++ {
		inst := &Instance{ID: fmt.Sprintf("inst-%d", i), State: "stopped", Command: []string{"true"}, CreatedAt: time.Now()}
		if err := db.SaveInstance(inst); err != nil {
			t.Fatalf("seed %s: %v", inst.ID, err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*writesEach)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("inst-%d", i)
			for n := 0; n < writesEach; n++ {
				var err error
				switch n % 3 {
				case 0:
					err = db.UpdatePublicPorts(id, map[int]int{80: 8000 + i})
				case 1:
					err = db.UpdateState(id, "running")
				default:
					err = db.SaveInstance(&Instance{ID: id, State: "running", Command: []string{"true"}, CreatedAt: time.Now()})
				}
				if err != nil {
					errs <- fmt.Errorf("%s write %d: %w", id, n, err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	for i := 0; i < writers; i++ {
		inst, err := db.GetInstance(fmt.Sprintf("inst-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if got := inst.PublicPorts[80]; got != 8000+i {
			t.Errorf("inst-%d: saved public port %d, want %d", i, got, 8000+i)
		}
	}
}
