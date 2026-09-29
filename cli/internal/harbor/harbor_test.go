package harbor

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/repo"
	"github.com/alternayte/casebox/cli/internal/sandbox"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// baseTree is a tar stream like git archive's: a pax global header with the commit id first.
func baseTree(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, n := range names {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: n, Mode: 0o644, Size: int64(len(files[n])), ModTime: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

const storeTestAtBase = "package store\n\nimport \"testing\"\n\nfunc TestGet(t *testing.T) {}\n"

// testPatch rewrites the held-out store_test.go and adds store/put_test.go.
const testPatch = `diff --git a/store/store_test.go b/store/store_test.go
--- a/store/store_test.go
+++ b/store/store_test.go
@@ -3,3 +3,5 @@ package store
 import "testing"
 
 func TestGet(t *testing.T) {}
+
+func TestGetMissing(t *testing.T) {}
diff --git a/store/put_test.go b/store/put_test.go
new file mode 100644
--- /dev/null
+++ b/store/put_test.go
@@ -0,0 +1,5 @@
+package store
+
+import "testing"
+
+func TestPut(t *testing.T) { Put("k", "v") }
`

const sourcePatch = `diff --git a/store/store.go b/store/store.go
--- a/store/store.go
+++ b/store/store.go
@@ -1 +1,3 @@
 package store
+
+func Put(key, value string) {}
`

func cannedTask(t *testing.T, commands []repo.TestCommand) Task {
	t.Helper()
	rec := repo.Recipe{
		Image:     "golang:1.26-bookworm",
		Install:   []string{"go mod download"},
		Lockfiles: []string{"go.mod", "go.sum"},
		Test:      commands,
		Services:  map[string]repo.Service{"postgres": {Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "casebox"}}},
	}
	var o Oracle
	o.Kind = "capability"
	o.Tests.FailToPass = []string{"example.com/shop/store::TestPut"}
	o.Tests.PassToPass = []string{"example.com/shop/store::TestGet"}
	o.TestFiles = []string{"store/store_test.go", "store/put_test.go"}
	return Task{
		Case: Case{
			ID: "0a1b2c3d4e5f60718293", Workspace: "shop", Kind: "capability", Scope: "single",
			Source: "github.com/acme/shop#7", WorkItem: "wi:jira:SHOP-1", FailToPass: 1, PassToPass: 1,
			Drift: true, Weight: 0.5, Seconds: 312.4,
			Instruction: "Add a way to store a value under a key.\n\nSay \"done\" when it works.\n",
			Signatures:  []string{"func Put(key, value string)"},
		},
		Oracle: o,
		Recipe: rec,
		Env: sandbox.EnvSpec{
			Image: rec.Image, Install: rec.Install,
			Context:  map[string][]byte{"go.mod": []byte("module example.com/shop\n")},
			Services: []sandbox.Service{{Name: "postgres", Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "casebox"}}},
		},
		Base: baseTree(t, map[string]string{
			"go.mod":               "module example.com/shop\n",
			"store/store.go":       "package store\n",
			"store/store_test.go":  storeTestAtBase,
			".casebox/casebox.yml": "version: 1\n",
			"vendor/x/.git/HEAD":   "ref: refs/heads/main\n",
			"docs/guide.md":        "a guide\n",
		}),
		SourcePatch: []byte(sourcePatch),
		TestPatch:   []byte(testPatch),
	}
}

// readTask reads every file of an exported task but the verdict program's source, which is the
// oracle package's own.
func readTask(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "environment/verdict/") {
			return nil
		}
		body, err := os.ReadFile(p)
		out[rel] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The exported task is pinned file by file; run with -update to accept a deliberate change.
func TestTheExportWritesTheCaseAsAHarborTask(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task")
	task := cannedTask(t, []repo.TestCommand{
		{Command: "go test -json ./... > /results/go-test.json", Results: oracle.GoTestJSON, Timeout: repo.Duration(15 * time.Minute)},
		{Command: "go vet ./..."},
	})
	if err := Write(dir, task); err != nil {
		t.Fatal(err)
	}
	got := readTask(t, dir)
	golden := filepath.Join("testdata", "golden")
	if *update {
		if err := os.RemoveAll(golden); err != nil {
			t.Fatal(err)
		}
		for name, body := range got {
			target := filepath.Join(golden, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := readTask(t, golden)
	for name, body := range want {
		if got[name] != body {
			t.Errorf("%s differs from the golden file\n got:\n%s\nwant:\n%s", name, got[name], body)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s is not in the golden files", name)
		}
	}

	for _, name := range []string{"environment/verdict/go.mod", "environment/verdict/main.go", "environment/verdict/oracle/oracle.go"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Errorf("the verdict program lacks %s: %v", name, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "environment", "verdict", "oracle", "*_test.go")); len(matches) > 0 {
		t.Errorf("the verdict program carries test files: %v", matches)
	}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"tests/test.sh", "solution/solve.sh"} {
			info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("%s is not executable: %v %v", name, info.Mode(), err)
			}
		}
	}
}

func TestTheVerifierTimeoutIsThreeTimesTheSlowestRunAndAtLeastTenMinutes(t *testing.T) {
	for seconds, want := range map[float64]int{0: 600, 199.9: 600, 200.1: 601, 900: 2700} {
		if got := VerifierTimeout(seconds); got != want {
			t.Errorf("VerifierTimeout(%v) = %d, want %d", seconds, got, want)
		}
	}
}

func TestRegistriesComeFromTheRecipesLanguages(t *testing.T) {
	cases := []struct {
		recipe repo.Recipe
		want   string
	}{
		{repo.Recipe{Image: "mcr.microsoft.com/dotnet/sdk:10.0", Test: []repo.TestCommand{{Command: "dotnet test --logger trx"}}}, "api.nuget.org"},
		{repo.Recipe{Image: "node:22", Install: []string{"npm ci"}, Lockfiles: []string{"web/package-lock.json"}}, "registry.npmjs.org registry.yarnpkg.com"},
		{repo.Recipe{Image: "python:3.12", Test: []repo.TestCommand{{Command: "pytest --junitxml=/results/junit.xml"}}}, "files.pythonhosted.org pypi.org"},
		// "javascript" is not Java, and a node_modules folder is not a Node image.
		{repo.Recipe{Image: "alpine:3", Install: []string{"echo javascript node_modules"}}, ""},
	}
	for _, c := range cases {
		if got := strings.Join(Registries(c.recipe), " "); got != c.want {
			t.Errorf("Registries(%+v) = %q, want %q", c.recipe, got, c.want)
		}
	}
}

// tests/test.sh runs on canned result files of each format: it restores the held-out file the
// agent changed, applies the held-out tests, runs the commands, and the verdict program built
// from the exported source decides.
func TestTheExportedVerifierDecidesFromEachResultsFormat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tests/test.sh is a POSIX shell script; Harbor runs it in a Linux container")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal("the test builds the verdict program and needs go on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("tests/test.sh applies the held-out tests with git")
	}
	fixtures, err := filepath.Abs(filepath.Join("..", "oracle", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	formats := []struct {
		format, file, results string
	}{
		{oracle.GoTestJSON, "go-test-clean.json", "go-test.json"},
		{oracle.TRX, "xunit.trx", "run.trx"},
		{oracle.JUnit, "pytest.xml", "junit.xml"},
	}
	verdict, built := filepath.Join(t.TempDir(), "casebox-verdict"), false
	for _, f := range formats {
		t.Run(f.format, func(t *testing.T) {
			fixture := filepath.Join(fixtures, f.file)
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			run, err := oracle.Parse(f.format, data)
			if err != nil || run.Error != "" {
				t.Fatalf("the fixture %s is not a clean run: %v %s", f.file, err, run.Error)
			}
			var passing, failing []string
			for _, r := range run.Results {
				if r.Status == oracle.Passed {
					passing = append(passing, r.ID)
				} else {
					failing = append(failing, r.ID)
				}
			}
			if len(passing) < 2 {
				t.Fatalf("the fixture %s has %d passing tests, want 2 or more", f.file, len(passing))
			}

			// The command leaves the fixture as its results only when the held-out tests applied.
			command := `test -f store/put_test.go && grep -q TestGetMissing store/store_test.go && mkdir -p "$CASEBOX_RESULTS_DIR/out" && cp ` + shellQuote(fixture) + ` "$CASEBOX_RESULTS_DIR/out/` + f.results + `"`
			task := cannedTask(t, []repo.TestCommand{{Command: command, Results: f.format}, {Command: "echo a command without results"}})
			dir := t.TempDir()
			if err := Write(dir, task); err != nil {
				t.Fatal(err)
			}
			if !built {
				built = true
				build := exec.Command(goBin, "build", "-o", verdict, ".")
				build.Dir = filepath.Join(dir, "environment", "verdict")
				build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
				if out, err := build.CombinedOutput(); err != nil {
					t.Fatalf("build the exported verdict program: %v\n%s", err, out)
				}
			}

			decide := func(t *testing.T, failToPass, passToPass []string) (string, string) {
				t.Helper()
				tests := filepath.Join(dir, "tests")
				oracleJSON := `{"failToPass":` + jsonList(failToPass) + `,"passToPass":` + jsonList(passToPass) + `}`
				if err := os.WriteFile(filepath.Join(tests, "oracle.json"), []byte(oracleJSON), 0o644); err != nil {
					t.Fatal(err)
				}
				// The agent's working directory: the sealed base, where the agent rewrote a
				// held-out file; the verifier restores it before the held-out tests apply.
				work := t.TempDir()
				if err := os.CopyFS(work, os.DirFS(filepath.Join(dir, "environment", "base"))); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(work, "store", "store_test.go"), []byte("package store // the agent's version\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				logs := t.TempDir()
				cmd := exec.Command("sh", filepath.Join(tests, "test.sh"))
				cmd.Env = append(os.Environ(),
					"CASEBOX_TESTS_DIR="+tests, "CASEBOX_LOGS_DIR="+logs, "CASEBOX_RESULTS_DIR="+t.TempDir(),
					"CASEBOX_WORKDIR="+work, "CASEBOX_VERDICT="+verdict)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("test.sh: %v\n%s", err, out)
				}
				reward, err := os.ReadFile(filepath.Join(logs, "reward.txt"))
				if err != nil {
					t.Fatalf("no reward.txt: %v\n%s", err, out)
				}
				counts, err := os.ReadFile(filepath.Join(logs, "reward.json"))
				if err != nil {
					t.Fatalf("no reward.json: %v\n%s", err, out)
				}
				return strings.TrimSpace(string(reward)), strings.TrimSpace(string(counts))
			}

			reward, counts := decide(t, passing[:1], passing[1:])
			want := `{"reward":1,"failToPass":1,"failToPassPassed":1,"passToPass":` + strconv.Itoa(len(passing)-1) + `,"passToPassPassed":` + strconv.Itoa(len(passing)-1) + `}`
			if reward != "1" || counts != want {
				t.Errorf("every test passes: reward %s, %s; want 1, %s", reward, counts, want)
			}
			if len(failing) > 0 {
				reward, counts = decide(t, append([]string{failing[0]}, passing[:1]...), nil)
				want = `{"reward":0,"failToPass":2,"failToPassPassed":1,"passToPass":0,"passToPassPassed":0}`
				if reward != "0" || counts != want {
					t.Errorf("a fail-to-pass test fails: reward %s, %s; want 0, %s", reward, counts, want)
				}
			}
			reward, counts = decide(t, []string{"missing::TestNowhere"}, passing)
			if reward != "0" || !strings.Contains(counts, `"failToPassPassed":0`) {
				t.Errorf("a fail-to-pass test is missing from the results: reward %s, %s; want 0", reward, counts)
			}
		})
	}
}

func jsonList(ids []string) string {
	data, _ := json.Marshal(ids)
	return string(data)
}
