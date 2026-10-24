package flg

import (
	"sync"
	"testing"

	"github.com/websines/structor-harness/record"
)

// TestStoreIsSafeForConcurrentUse pins the lock that guards the log.
//
// This test did not exist while a UI was being built on top of the store, which
// is why `go test -race` stayed green through a real data race: the race
// detector only reports races it OBSERVES, and nothing in the suite was
// concurrent. A lock with no concurrent test is an assertion, not a guarantee.
func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	cas, err := record.NewLocalCAS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(cas)

	const writers, readers, n = 4, 4, 40
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				if _, err := st.AppendObligation(Obligation{
					ID: string(rune('A'+w)) + string(rune('0'+i%10)), Title: "concurrent",
				}); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				_ = st.ManifestObjects()
				if _, _, _, err := st.Project(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
