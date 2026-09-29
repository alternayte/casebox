package cases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox/docker"
)

func recipeWith(image, command string) repo.Recipe {
	return repo.Recipe{Image: image, Test: []repo.TestCommand{{Command: command}}}
}

// blobs is an in-memory blob store in place of the server's.
type blobs map[string][]byte

func (b blobs) put(_ context.Context, _ string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	b[hash] = append([]byte(nil), data...)
	return hash, nil
}

// End to end against Docker: a tiny Go module whose base has a bug a new test catches, and a merge
// that fixes it. Validation must find the fail-to-pass test, pass it 3 of 3, and flag the harness
// drift of the current AGENTS.md.
func TestValidateAgainstDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	p := docker.New()
	check, stop := context.WithTimeout(ctx, 30*time.Second)
	err := p.Check(check)
	stop()
	if err != nil {
		t.Skipf("validation needs Docker, which is not usable here: %v", err)
	}

	r := newRepo(t)
	r.commit("base", map[string]string{
		"go.mod":              "module example.com/calc\n\ngo 1.22\n",
		"calc/calc.go":        "package calc\n\n// Add adds.\nfunc Add(a, b int) int { return a - b }\n",
		"calc/calc_test.go":   "package calc\n\nimport \"testing\"\n\nfunc TestZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n",
		"other/other.go":      "package other\n\nfunc One() int { return 1 }\n",
		"other/other_test.go": "package other\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) {\n\tif One() != 1 {\n\t\tt.Fatal(\"one\")\n\t}\n}\n",
	})
	merge := r.pull("fix-add", map[string]string{
		"calc/calc.go":      "package calc\n\n// Add adds.\nfunc Add(a, b int) int { return a + b }\n",
		"calc/calc_test.go": "package calc\n\nimport \"testing\"\n\nfunc TestZero(t *testing.T) {\n\tif Add(0, 0) != 0 {\n\t\tt.Fatal(\"zero\")\n\t}\n}\n\nfunc TestAdd(t *testing.T) {\n\tif got := Add(2, 3); got != 5 {\n\t\tt.Fatalf(\"Add(2, 3) = %d\", got)\n\t}\n}\n",
	})
	base := r.git("rev-parse", merge+"^1")
	// The current harness names a file the base has and a recipe it lacks.
	r.commit("harness", map[string]string{"AGENTS.md": "Read `calc/calc.go` first, then run `just test`.\n"})

	store := blobs{}
	res, err := validate(ctx, p, opener(map[string]*gitRepo{apiRepo: r}), store.put, ValidatePayload{
		CaseID: "c1",
		Kind:   Capability,
		Recipe: repo.Recipe{
			Image:   "golang:1.26.2-alpine",
			Install: []string{"apk add --no-cache git"},
			Test:    []repo.TestCommand{{Command: "go test -json ./... > /results/go-test.json", Results: "go-test-json"}},
		},
		Repos: []CaseRepo{{Repo: apiRepo, Base: base, Merged: ptr(merge), Role: RoleSealed}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("validation failed: %s: %s", res.Reason, res.Detail)
	}
	if res.FailToPass < 1 || res.PassToPass < 1 || res.Seconds <= 0 {
		t.Errorf("result %+v", res)
	}
	if !res.Drift || res.Weight != 0.5 || res.Retire {
		t.Errorf("drift %v, weight %v, retire %v; want drift at weight 0.5 without retiring (%s)", res.Drift, res.Weight, res.Retire, res.Detail)
	}

	var o Oracle
	if err := json.Unmarshal(store[res.Oracle], &o); err != nil {
		t.Fatalf("the oracle blob: %v", err)
	}
	if strings.Join(o.Tests.FailToPass, ",") != "example.com/calc/calc::TestAdd" {
		t.Errorf("fail-to-pass %v", o.Tests.FailToPass)
	}
	// Pass-to-pass holds the touched package only.
	if strings.Join(o.Tests.PassToPass, ",") != "example.com/calc/calc::TestZero" {
		t.Errorf("pass-to-pass %v", o.Tests.PassToPass)
	}
	if strings.Join(o.TestFiles, ",") != "calc/calc_test.go" || o.Kind != Capability || len(o.Commands) != 1 || o.Commands[0].Results != "go-test-json" {
		t.Errorf("oracle %+v", o)
	}
	src, tests := string(store[o.SourcePatch]), string(store[o.TestPatch])
	if !strings.Contains(src, "calc/calc.go") || strings.Contains(src, "calc_test.go") || !strings.Contains(tests, "TestAdd") || strings.Contains(tests, "calc/calc.go") {
		t.Errorf("patches:\n%s\n%s", src, tests)
	}
}
