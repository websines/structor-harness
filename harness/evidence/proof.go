package evidence

import (
	"fmt"
	"github.com/websines/structor-harness/flg"
	"github.com/websines/structor-harness/reason"
)

func matchingProof(verdict flg.Verdict, state flg.OblState, proof *reason.ProofNode) (*reason.ProofNode, error) {
	if state != verdict.State {
		return nil, fmt.Errorf("evidence: proof state %s disagrees with verdict %s", state, verdict.State)
	}
	return proof, nil
}
