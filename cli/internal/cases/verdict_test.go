package cases

import (
	"strings"
	"testing"
	"time"

	"github.com/alternayte/casebox/cli/internal/oracle"
	"github.com/alternayte/casebox/cli/internal/runner"
)

const (
	testA = "example.com/m/store::TestA"
	testB = "example.com/m/store::TestB"
	testC = "example.com/m/other::TestC"
)

// ran is a canned verifier run: statuses by test ID.
func ran(tests map[string]oracle.Status) runOutcome {
	o := runOutcome{Applied: true, Duration: time.Minute}
	for id, s := range tests {
		o.Run.Results = append(o.Run.Results, oracle.Result{ID: id, Status: s})
	}
	return o
}

func three(o runOutcome) []runOutcome { return []runOutcome{o, o, o} }

func TestDecide(t *testing.T) {
	pass, fail := oracle.Passed, oracle.Failed
	base := ran(map[string]oracle.Status{testA: fail, testB: pass, testC: pass})
	good := ran(map[string]oracle.Status{testA: pass, testB: pass, testC: pass})
	store := scopeOf([]string{"store"})

	t.Run("fail-to-pass and pass-to-pass in the touched packages", func(t *testing.T) {
		v := decide(Capability, base, three(good), store)
		if v.Reason != "" || strings.Join(v.FailToPass, ",") != testA || strings.Join(v.PassToPass, ",") != testB {
			t.Fatalf("%+v", v)
		}
		if v.Slowest != time.Minute {
			t.Errorf("slowest %s", v.Slowest)
		}
	})
	t.Run("a test that did not exist at the base is fail-to-pass", func(t *testing.T) {
		crashed := runOutcome{Applied: true, Run: oracle.Run{Error: "store: build failed", Results: []oracle.Result{{ID: testC, Status: pass}}}}
		v := decide(Capability, crashed, three(good), store)
		if v.Reason != "" || strings.Join(v.FailToPass, ",") != testA+","+testB || len(v.PassToPass) != 0 {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("a fail-to-pass test that flakes drops the case", func(t *testing.T) {
		flaky := ran(map[string]oracle.Status{testA: fail, testB: pass, testC: pass})
		v := decide(Capability, base, []runOutcome{good, flaky, good}, store)
		if v.Reason != ReasonFlaky || !strings.Contains(v.Detail, testA) {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("a pass-to-pass test that flakes drops the case; one outside the scope does not", func(t *testing.T) {
		v := decide(Capability, base, []runOutcome{good, ran(map[string]oracle.Status{testA: pass, testB: fail, testC: pass}), good}, store)
		if v.Reason != ReasonFlaky {
			t.Fatalf("%+v", v)
		}
		v = decide(Capability, base, []runOutcome{good, ran(map[string]oracle.Status{testA: pass, testB: pass, testC: fail}), good}, store)
		if v.Reason != "" || strings.Join(v.PassToPass, ",") != testB {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("tests that already pass at the base", func(t *testing.T) {
		v := decide(Capability, good, three(good), store)
		if v.Reason != ReasonFailToPassPassedAtBase {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("no fail-to-pass", func(t *testing.T) {
		failing := ran(map[string]oracle.Status{testA: fail})
		if v := decide(Regression, failing, three(failing), store); v.Reason != ReasonNoFailToPass {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("a steering case may have no fail-to-pass", func(t *testing.T) {
		if v := decide(Steering, good, three(good), store); v.Reason != "" || len(v.FailToPass) != 0 || len(v.PassToPass) != 2 {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("merged runs that crash", func(t *testing.T) {
		crashed := runOutcome{Applied: true, Duration: time.Second, Run: oracle.Run{Error: "build failed"}}
		if v := decide(Capability, base, three(crashed), store); v.Reason != ReasonBuild {
			t.Fatalf("all crashed: %+v", v)
		}
		if v := decide(Capability, base, []runOutcome{good, crashed, good}, store); v.Reason != ReasonFlaky {
			t.Fatalf("one crashed: %+v", v)
		}
	})
	t.Run("the slowest run is over the limit", func(t *testing.T) {
		slow := good
		slow.Duration = 11 * time.Minute
		if v := decide(Capability, base, []runOutcome{good, slow, good}, store); v.Reason != ReasonTooSlow {
			t.Fatalf("%+v", v)
		}
		timedOut := good
		timedOut.TimedOut = true
		if v := decide(Capability, base, []runOutcome{good, good, timedOut}, store); v.Reason != ReasonTooSlow {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("a command without results counts by its exit code only", func(t *testing.T) {
		withExit := func(o runOutcome, code int) runOutcome {
			o.Exits = []exitResult{{Command: "npm run lint", ExitCode: code}}
			return o
		}
		if v := decide(Capability, base, three(withExit(good, 1)), store); v.Reason != ReasonBuild {
			t.Fatalf("always failing: %+v", v)
		}
		if v := decide(Capability, base, []runOutcome{withExit(good, 0), withExit(good, 2), withExit(good, 0)}, store); v.Reason != ReasonFlaky {
			t.Fatalf("failing once: %+v", v)
		}
		if v := decide(Capability, base, three(withExit(good, 0)), store); v.Reason != "" || len(v.FailToPass) != 1 {
			t.Fatalf("passing: %+v", v)
		}
		exitOnly := withExit(runOutcome{Applied: true}, 0)
		if v := decide(Capability, withExit(runOutcome{Applied: true}, 1), three(exitOnly), store); v.Reason != ReasonNoFailToPass || !strings.Contains(v.Detail, "per-test") {
			t.Fatalf("exit codes only: %+v", v)
		}
	})
}

func TestScope(t *testing.T) {
	for _, c := range []struct {
		dirs []string
		id   string
		want bool
	}{
		{[]string{"store"}, "example.com/m/store::TestA", true},
		{[]string{"internal/store"}, "example.com/m/internal/store::TestA", true},
		{[]string{"internal/store"}, "example.com/m/store::TestA", true},
		{[]string{"calc"}, "example.com/calc/other::TestA", false},
		{[]string{"calc"}, "example.com/calc/calc/sub::TestA", false},
		{[]string{"src/Api.Tests"}, "Api.Tests.StoreTests::GetsAnItem", true},
		{[]string{"src/Api"}, "Api.Tests.StoreTests::GetsAnItem", true},
		{[]string{"src/Web"}, "Api.Tests.StoreTests::GetsAnItem", false},
		{[]string{"src/test/java/com/acme/store"}, "com.acme.store.StoreTest::find", true},
		{[]string{"src/test/java/com/acme/store"}, "com.acme.cart.CartTest::add", false},
		{[]string{"pkg"}, "tests.test_store::test_load", false},
		{[]string{"."}, "anything::at_all", true},
		{[]string{"tests"}, "anything::at_all", true},
	} {
		in := scopeOf(c.dirs)
		if got := in == nil || in(c.id); got != c.want {
			t.Errorf("%v, %s: in scope %v, want %v", c.dirs, c.id, got, c.want)
		}
	}
}

// outcome reads each command's results in its format, and keeps only the files that format has.
func TestOutcome(t *testing.T) {
	junit := `<testsuite name="s"><testcase classname="com.acme.StoreTest" name="find" time="0.1"/></testsuite>`
	res := runner.Result{Applied: true, Commands: []runner.CommandResult{
		{Command: "mvn test", Results: "junit", ExitCode: 0, Duration: time.Second, Files: map[string][]byte{
			"surefire-reports/TEST-com.acme.StoreTest.xml": []byte(junit),
			"surefire-reports/com.acme.StoreTest.txt":      []byte("Tests run: 1"),
		}},
		{Command: "npm run lint", ExitCode: 3, Duration: time.Second, Output: "lint failed"},
		{Command: "go test -json ./...", Results: "go-test-json", ExitCode: 1, Duration: time.Second},
	}}
	o, err := outcome(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Run.Results) != 1 || o.Run.Results[0].ID != "com.acme.StoreTest::find" || o.Run.Results[0].Status != oracle.Passed {
		t.Errorf("results %+v", o.Run.Results)
	}
	if !strings.Contains(o.Run.Error, "go test -json ./...: no results files") {
		t.Errorf("a command with no results file is a crash: %q", o.Run.Error)
	}
	if len(o.Exits) != 1 || o.Exits[0].ExitCode != 3 || o.Duration != 3*time.Second {
		t.Errorf("exits %+v, duration %s", o.Exits, o.Duration)
	}
}

// The verifier's allow-list never reaches a git host.
func TestRegistriesPassTheEgressCheck(t *testing.T) {
	all := Registries(recipeWith("mcr.microsoft.com/dotnet/sdk:10.0", "go test ./... && npm test && pytest && mvn test"))
	if len(all) != 12 {
		t.Errorf("registries %v", all)
	}
	if err := runner.CheckEgress(all); err != nil {
		t.Fatal(err)
	}
	if got := Registries(recipeWith("golang:1.26", "go test -json ./... > /results/go-test.json")); strings.Join(got, ",") != "proxy.golang.org,sum.golang.org" {
		t.Errorf("go registries %v", got)
	}
}
