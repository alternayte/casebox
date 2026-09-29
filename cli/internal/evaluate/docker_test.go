package evaluate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/agents"
	"github.com/alternayte/casebox/cli/internal/cases"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
)

// blobStore is an in-memory blob store in place of the server's.
type blobStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (b *blobStore) put(_ context.Context, _ string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blobs[hash] = append([]byte(nil), data...)
	return hash, nil
}

func (b *blobStore) get(_ context.Context, hash string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[hash]
	if !ok {
		return nil, fmt.Errorf("no blob %s", hash)
	}
	return data, nil
}

// End to end against Docker, with the command agent and no model: a tiny Go module whose base has
// a bug that a held-out test catches. An agent that fixes the bug gives a diff that passes the
// verifier; an agent that does nothing fails it.
func TestRunAndVerifyAgainstDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	p := docker.New()
	check, stop := context.WithTimeout(ctx, 30*time.Second)
	err := p.Check(check)
	stop()
	if err != nil {
		t.Skipf("the run and verify test needs Docker, which is not usable here: %v", err)
	}

	const name = "example.com/acme/calc"
	r := newRepo(t)
	zero := "package calc\n\nimport \"testing\"\n\nfunc TestZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n"
	base := r.commit("base", map[string]string{
		"go.mod":            "module example.com/calc\n\ngo 1.22\n",
		"AGENTS.md":         "Run go test before you say done.\n",
		"calc/calc.go":      "package calc\n\n// Add adds.\nfunc Add(a, b int) int { return a - b }\n",
		"calc/calc_test.go": zero,
	})
	merged := r.commit("fix add", map[string]string{
		"calc/calc.go":      "package calc\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n",
		"calc/calc_test.go": zero + "\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2, 3) = %d\", got)\n\t}\n}\n",
	})
	testPatch := []byte(r.git("diff", "--binary", "--full-index", "--no-renames", base, merged, "--", "calc/calc_test.go") + "\n")
	r.git("reset", "-q", "--hard", base) // the default branch is the base again, so its harness is the base's

	store := &blobStore{blobs: map[string][]byte{}}
	testPatchHash, _ := store.put(ctx, "", testPatch)
	o := fmt.Sprintf(`{"kind":"capability","tests":{"failToPass":["example.com/calc/calc::TestAdd"],"passToPass":["example.com/calc/calc::TestZero"]},
		"testFiles":["calc/calc_test.go"],"testPatch":%q,"assertions":[{"kind":"forbidden_file","path":"go.mod"}],"judge":[],
		"commands":[{"command":"go test -json ./... > /results/go-test.json","results":"go-test-json"}]}`, testPatchHash)
	oracleHash, _ := store.put(ctx, "", []byte(o))

	d := deps{
		provider: p,
		open: func(_ context.Context, repo string) (string, error) {
			if repo != name {
				return "", fmt.Errorf("no mirror of %s", repo)
			}
			return r.dir, nil
		},
		put: store.put,
		get: store.get,
		env: map[string]string{}, // no model key
	}
	rec := repo.Recipe{
		Image:   "golang:1.26.2-alpine",
		Install: []string{"apk add --no-cache git"},
		Test:    []repo.TestCommand{{Command: "go test -json ./... > /results/go-test.json", Results: "go-test-json"}},
	}
	repos := []cases.CaseRepo{{Repo: name, Base: base, Merged: &merged, Role: cases.RoleSealed}}
	one := func(v float64) *float64 { return &v }
	payload := func(template string) RunPayload {
		return RunPayload{
			EvaluationID: "e1", RunID: "r1", CaseID: "c1", Side: "candidate", Repeat: 1,
			Spec: agents.Spec{Agent: agents.CommandCLI, AgentVersion: "1.0.0", Model: "none-1", Harness: "main",
				Command: &agents.Command{Template: template, LogFormat: agents.LogFormatNone}},
			Recipe: rec, Repos: repos, Oracle: oracleHash,
			Instruction: "Make Add add its arguments.",
			Prices:      map[string]repo.Price{"none-1": {Input: one(1), Output: one(2)}},
		}
	}
	verifyOf := func(t *testing.T, a RunAnswer) VerifyAnswer {
		t.Helper()
		v, err := verify(ctx, d, VerifyPayload{EvaluationID: "e1", RunID: a.RunID, CaseID: "c1", Recipe: rec, Repos: repos, Oracle: oracleHash, Diff: a.Diff, Trace: a.Trace})
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		return v
	}

	t.Run("an agent that fixes the bug passes", func(t *testing.T) {
		// The agent reads its instruction, cannot see the held-out test, and fixes the bug.
		fix := `grep -q 'Make Add add' {instruction_file} && test ! -e calc/calc_test.go && test -f AGENTS.md && sed -i 's/a - b/a + b/' calc/calc.go`
		a, err := run(ctx, d, payload(fix))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if a.Diff == nil || a.Trace == nil || a.Log != nil || a.HarnessHash == nil || len(*a.HarnessHash) != 64 || a.TimedOut || a.TokenCapExceeded || a.CostUSD != 0 || a.Seconds <= 0 {
			t.Fatalf("run answer %+v", a)
		}
		diff, _ := store.get(ctx, *a.Diff)
		if !strings.Contains(string(diff), "+func Add(a, b int) int { return a + b }") || strings.Contains(string(diff), "calc_test.go") {
			t.Fatalf("the diff does not hold the fix alone:\n%s", diff)
		}
		if a.Model == nil || *a.Model != "none-1" {
			t.Fatalf("model %v", a.Model)
		}

		v := verifyOf(t, a)
		want := TestCounts{FailToPass: TestCount{Passed: 1, Total: 1}, PassToPass: TestCount{Passed: 1, Total: 1}}
		if !v.Applied || v.Tests == nil || *v.Tests != want || !v.Passed || len(v.Failed) != 0 || v.Judge != nil ||
			len(v.Assertions) != 1 || !v.Assertions[0].Passed {
			t.Fatalf("verify answer %+v, tests %+v", v, v.Tests)
		}
	})

	t.Run("an agent that does nothing fails", func(t *testing.T) {
		a, err := run(ctx, d, payload(`test -f {instruction_file}`))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		v := verifyOf(t, a)
		if !v.Applied || v.Passed || v.Tests == nil || v.Tests.FailToPass != (TestCount{Passed: 0, Total: 1}) || len(v.Failed) != 1 {
			t.Fatalf("verify answer %+v, tests %+v", v, v.Tests)
		}
	})
}

// The cap watcher reads a streaming agent's stdout while it runs and stops it at the crossing:
// the agent below would print usage for ten seconds and then leave a file behind.
func TestTokenCapStopsAStreamingAgentAgainstDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := docker.New()
	check, stop := context.WithTimeout(ctx, 30*time.Second)
	err := p.Check(check)
	stop()
	if err != nil {
		t.Skipf("the token cap test needs Docker, which is not usable here: %v", err)
	}
	image, err := p.Prepare(ctx, sandbox.EnvSpec{Image: "alpine:3.20"})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := p.Start(ctx, image, sandbox.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb) })

	a, err := agents.For(agents.Spec{Agent: agents.ClaudeCode, AgentVersion: agents.Flags[0].Min, Model: "claude-sonnet-5", Harness: "none"})
	if err != nil {
		t.Fatal(err)
	}
	script := `for i in 1 2 3 4 5 6 7 8 9 10; do echo "{\"type\":\"assistant\",\"message\":{\"id\":\"m$i\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0,\"cache_read_input_tokens\":5000}}}"; sleep 1; done; touch /tmp/finished`
	out, err := runAgent(ctx, p, sb, a, []string{"sh", "-c", script}, "/tmp/stdout.jsonl", time.Minute, 250)
	if err != nil {
		t.Fatal(err)
	}
	if !out.capped || out.timedOut || out.seconds >= 9 || agents.Tokens(out.watched) <= 250 {
		t.Fatalf("outcome %+v", out)
	}
	if len(out.stdout) == 0 {
		t.Fatal("the stdout was not copied out")
	}
	res, err := p.Exec(ctx, sb, sandbox.Command{Args: []string{"sh", "-c", "sleep 10; test ! -e /tmp/finished"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("the agent kept running after the cap: %v, exit %d", err, res.ExitCode)
	}
}
