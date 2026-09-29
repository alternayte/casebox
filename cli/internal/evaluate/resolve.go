package evaluate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alternayte/casebox/cli/internal/agents"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/worker"
)

// ResolvePayload is the payload of a harness.resolve job: the baseline spec and every case it may
// score (docs/specs/harness-ci.md, "The baseline").
type ResolvePayload struct {
	CIRun string      `json:"ciRun"`
	Spec  agents.Spec `json:"spec"`
	Cases []struct {
		CaseID string           `json:"caseId"`
		Repos  []cases.CaseRepo `json:"repos"`
	} `json:"cases"`
}

// ResolveAnswer is the harness hash each case gets from the spec: the hash a run of that case
// would report, computed from the mirrors without a sandbox.
type ResolveAnswer struct {
	Hashes map[string]string `json:"hashes"`
}

// Resolve runs a harness.resolve job.
func (j Jobs) Resolve(ctx context.Context, job worker.Job) (any, error) {
	var p ResolvePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || len(p.Cases) == 0 {
		return nil, worker.Permanent{Err: errors.New("the job names no case")}
	}
	return resolve(ctx, j.deps(), p)
}

func resolve(ctx context.Context, d deps, p ResolvePayload) (ResolveAnswer, error) {
	answer := ResolveAnswer{Hashes: map[string]string{}}
	for _, c := range p.Cases {
		sealed, err := openSealed(ctx, d, c.Repos)
		if err != nil {
			return answer, fmt.Errorf("case %s: %w", c.CaseID, err)
		}
		h, err := sideHarness(ctx, d, sealed, p.Spec)
		if err != nil {
			return answer, fmt.Errorf("case %s: %w", c.CaseID, err)
		}
		answer.Hashes[c.CaseID] = h.hash
	}
	return answer, nil
}
