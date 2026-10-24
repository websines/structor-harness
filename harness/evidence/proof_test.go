package evidence

import (
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/reason"
	"testing"
)

func TestMismatchedProofIsNotAttached(t *testing.T) {
	proof := &reason.ProofNode{}
	for _, verdictState := range flg.AllOblStates() {
		for _, proofState := range flg.AllOblStates() {
			result, err := matchingProof(flg.Verdict{State: verdictState}, proofState, proof)
			if verdictState != proofState {
				if err == nil || result != nil {
					t.Fatalf("attached %s proof to %s verdict", proofState, verdictState)
				}
			} else if err != nil || result != proof {
				t.Fatalf("matching proof rejected: %v", err)
			}
		}
	}
}
