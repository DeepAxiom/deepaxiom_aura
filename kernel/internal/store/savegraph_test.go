package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Registering a graph while the node is writing anything else must not fail.
//
// SaveGraph reads the newest revision and then writes the next one. In a
// deferred transaction that is a read lock upgraded to a write lock, and SQLite
// refuses the upgrade outright — "database is locked", without waiting for
// busy_timeout — when another connection committed in between. On a node that
// meant `POST /v1/graphs` answering 500 because an Expertise happened to
// register at the same moment.
func TestSaveGraphSurvivesConcurrentWrites(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	stop := make(chan struct{})
	var others sync.WaitGroup
	for w := 0; w < 4; w++ {
		others.Add(1)
		go func(w int) {
			defer others.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("expertise-%d", w)
				if err := st.UpsertSkill(id, "1.0.0", map[string]any{"id": id, "n": i}); err != nil {
					t.Errorf("UpsertSkill: %v", err)
					return
				}
			}
		}(w)
	}

	var failed atomic.Int64
	var first atomic.Value
	var savers sync.WaitGroup
	deadline := time.Now().Add(2 * time.Second)
	for g := 0; g < 4; g++ {
		savers.Add(1)
		go func(g int) {
			defer savers.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				ir := []byte(fmt.Sprintf(`{"graph_id":"g%d","rev":%d}`, g, i))
				if err := st.SaveGraph(fmt.Sprintf("g%d", g), ir); err != nil {
					failed.Add(1)
					first.CompareAndSwap(nil, err.Error())
				}
			}
		}(g)
	}
	savers.Wait()
	close(stop)
	others.Wait()

	if n := failed.Load(); n > 0 {
		t.Fatalf("%d SaveGraph calls failed under concurrent writes; first: %v", n, first.Load())
	}
}
