package oracle

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func statuses(run Run) map[string]Status {
	m := map[string]Status{}
	for _, r := range run.Results {
		if _, dup := m[r.ID]; dup {
			panic("duplicate result " + r.ID)
		}
		m[r.ID] = r.Status
	}
	return m
}

func TestParseFixtures(t *testing.T) {
	const calc, hang, slow = "example.com/shop/calc::", "example.com/shop/hang::", "example.com/shop/slow::"
	cases := []struct {
		file, format string
		want         map[string]Status
		// errorHas lists substrings of Run.Error; none means the run did not crash.
		errorHas []string
		// output pins a substring of one test's output.
		output map[string]string
		// seconds pins one test's duration.
		seconds map[string]float64
	}{
		{
			file: "go-test.json", format: GoTestJSON,
			want: map[string]Status{
				calc + "TestAdd":        Passed,
				calc + "TestSub":        Failed,
				calc + "TestSkip":       Skipped,
				calc + "TestTable":      Failed,
				calc + "TestTable/zero": Passed,
				calc + "TestTable/neg":  Failed,
				hang + "TestFine":       Passed,
				hang + "TestPanic":      Error,
				slow + "TestQuick":      Passed,
				slow + "TestStuck":      Error,
			},
			errorHas: []string{"example.com/shop/broken: build failed", "broken/broken.go:3:37: undefined: c"},
			output: map[string]string{
				calc + "TestSub":       "calc_test.go:14: Add(5, -3) = 2, want 3",
				calc + "TestSkip":      "needs a database",
				calc + "TestTable/neg": "got -2, want -3",
				hang + "TestPanic":     "panic: assignment to entry in nil map",
				slow + "TestStuck":     "panic: test timed out after 1s",
			},
		},
		{
			file: "go-test-clean.json", format: GoTestJSON,
			want: map[string]Status{
				calc + "TestAdd":        Passed,
				calc + "TestSkip":       Skipped,
				calc + "TestTable":      Passed,
				calc + "TestTable/zero": Passed,
			},
		},
		{
			file: "xunit.trx", format: TRX,
			want: map[string]Status{
				"Shop.Tests.CalcTests::Adds":                             Passed,
				"Shop.Tests.CalcTests::Subtracts":                        Failed,
				"Shop.Tests.CalcTests::NeedsDatabase":                    Skipped,
				"Shop.Tests.CalcTests::Divides(a: 6, b: 3, expected: 2)": Passed,
				"Shop.Tests.CalcTests::Divides(a: 1, b: 0, expected: 0)": Failed,
			},
			output: map[string]string{
				"Shop.Tests.CalcTests::Subtracts":     "Assert.Equal() Failure: Values differ",
				"Shop.Tests.CalcTests::NeedsDatabase": "needs a database",
			},
			seconds: map[string]float64{
				"Shop.Tests.CalcTests::Divides(a: 1, b: 0, expected: 0)": 1.25,
				"Shop.Tests.CalcTests::Adds":                             0.0021934,
			},
		},
		{
			file: "nunit.trx", format: TRX,
			want: map[string]Status{
				"Shop.Tests.MoneyTests::Rounds(2.5,2)":   Passed,
				"Shop.Tests.MoneyTests::Rounds(3.5,4)":   Failed,
				"Shop.Tests.MoneyTests::FormatsCurrency": Skipped,
			},
			output: map[string]string{"Shop.Tests.MoneyTests::Rounds(3.5,4)": "But was:  3"},
		},
		{
			file: "mstest-crash.trx", format: TRX,
			want: map[string]Status{
				"Shop.Tests.CalcTests::Divides (6,3,2)":     Passed,
				"Shop.Tests.CalcTests::Divides (1,0,0)":     Failed,
				"Shop.Tests.InventoryTests::SyncsInventory": Error,
			},
			errorHas: []string{"Test host process crashed : Stack overflow."},
			output:   map[string]string{"Shop.Tests.InventoryTests::SyncsInventory": "exceeded execution timeout period"},
			seconds:  map[string]float64{"Shop.Tests.InventoryTests::SyncsInventory": 30},
		},
		{
			file: "pytest.xml", format: JUnit,
			want: map[string]Status{
				"tests.test_calc::test_add":                  Passed,
				"tests.test_calc::test_divide[6-3-2]":        Passed,
				"tests.test_calc::test_divide[1-0-0]":        Failed,
				"tests.test_calc::test_needs_db":             Skipped,
				"tests.test_calc.TestRounding::test_half_up": Passed,
				"::tests.test_orders":                        Error,
			},
			output: map[string]string{
				"tests.test_calc::test_divide[1-0-0]": "E       ZeroDivisionError: division by zero",
				"tests.test_calc::test_needs_db":      "needs a database",
				"::tests.test_orders":                 "ImportError: cannot import name 'place'",
			},
			seconds: map[string]float64{"tests.test_calc::test_divide[1-0-0]": 0.002},
		},
		{
			file: "jest-junit.xml", format: JUnit,
			want: map[string]Status{
				"cart adds an item::cart adds an item":                         Passed,
				"cart totals › with a discount::cart totals › with a discount": Failed,
				"cart totals › with tax::cart totals › with tax":               Passed,
				"cart ships abroad::cart ships abroad":                         Skipped,
			},
			errorHas: []string{`suite "src/checkout.test.ts" failed to run`},
			output:   map[string]string{"cart totals › with a discount::cart totals › with a discount": "Expected: 90"},
		},
		{
			file: "TEST-com.example.shop.CalcTest.xml", format: JUnit,
			want: map[string]Status{
				"com.example.shop.CalcTest::adds":                      Passed,
				"com.example.shop.CalcTest::divides(int, int, int)[1]": Passed,
				"com.example.shop.CalcTest::divides(int, int, int)[2]": Error,
				"com.example.shop.CalcTest::subtracts":                 Failed,
				"com.example.shop.CalcTest::needsDatabase":             Skipped,
			},
			output: map[string]string{
				"com.example.shop.CalcTest::subtracts":                 "expected: <3> but was: <2>",
				"com.example.shop.CalcTest::divides(int, int, int)[2]": "java.lang.ArithmeticException: / by zero",
			},
			seconds: map[string]float64{"com.example.shop.CalcTest::divides(int, int, int)[2]": 0.011},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			run, err := Parse(tc.format, fixture(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if got := statuses(run); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("statuses:\n got %v\nwant %v", got, tc.want)
			}
			if len(tc.errorHas) == 0 && run.Error != "" {
				t.Errorf("unexpected run error: %s", run.Error)
			}
			for _, s := range tc.errorHas {
				if !strings.Contains(run.Error, s) {
					t.Errorf("run error lacks %q:\n%s", s, run.Error)
				}
			}
			byID := run.ByID()
			for id, s := range tc.output {
				if !strings.Contains(byID[id].Output, s) {
					t.Errorf("%s output lacks %q:\n%s", id, s, byID[id].Output)
				}
			}
			for id, s := range tc.seconds {
				if got := byID[id].Seconds; got < s-1e-9 || got > s+1e-9 {
					t.Errorf("%s seconds = %v, want %v", id, got, s)
				}
			}
			for _, r := range run.Results {
				if r.Status == Passed && r.Output != "" {
					t.Errorf("%s passed but kept output", r.ID)
				}
			}
		})
	}
}

func TestGoRepeatedRunLastWins(t *testing.T) {
	data := `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Test":"TestA","Output":"first run failed\n"}
{"Action":"fail","Package":"p","Test":"TestA","Elapsed":0.5}
{"Action":"run","Package":"p","Test":"TestA"}
{"Action":"output","Package":"p","Test":"TestA","Output":"--- PASS: TestA\n"}
{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.25}
{"Action":"fail","Package":"p","Elapsed":1}
`
	run, err := Parse(GoTestJSON, []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{{ID: "p::TestA", Status: Passed, Seconds: 0.25}}
	if !reflect.DeepEqual(run.Results, want) {
		t.Errorf("results = %+v, want %+v", run.Results, want)
	}
	// The package failed and its only test passed at the end: the failure is outside any test.
	if !strings.Contains(run.Error, "p: failed outside any test") {
		t.Errorf("run error = %q", run.Error)
	}
}

func TestGoTruncated(t *testing.T) {
	data := fixture(t, "go-test-clean.json")
	cut := data[:len(data)*2/3]
	run, err := Parse(GoTestJSON, cut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.Error, "example.com/shop/calc: the run stopped before the package finished") {
		t.Errorf("run error = %q", run.Error)
	}
}

func TestGoOutputCapped(t *testing.T) {
	line := `{"Action":"output","Package":"p","Test":"TestBig","Output":"` + strings.Repeat("x", 1000) + `\n"}` + "\n"
	data := `{"Action":"run","Package":"p","Test":"TestBig"}` + "\n" + strings.Repeat(line, 20) +
		`{"Action":"output","Package":"p","Test":"TestBig","Output":"the real failure\n"}` + "\n" +
		`{"Action":"fail","Package":"p","Test":"TestBig"}` + "\n" + `{"Action":"fail","Package":"p"}` + "\n"
	run, _ := Parse(GoTestJSON, []byte(data))
	out := run.ByID()["p::TestBig"].Output
	if len(out) > maxOutput {
		t.Errorf("output is %d bytes, cap is %d", len(out), maxOutput)
	}
	if !strings.HasSuffix(out, "the real failure\n") {
		t.Errorf("output lost its tail")
	}
}

func TestCrashedFiles(t *testing.T) {
	cases := []struct {
		name, format string
		data         string
		errorHas     string
	}{
		{"empty go", GoTestJSON, "", "no go test -json events"},
		{"go command error", GoTestJSON, "go: cannot find main module\n", "go: cannot find main module"},
		{"empty trx", TRX, "", "unreadable TRX file"},
		{"truncated trx", TRX, string(fixture(t, "xunit.trx")[:900]), "unreadable TRX file"},
		{"aborted trx", TRX, `<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"><ResultSummary outcome="Aborted"/></TestRun>`, "outcome Aborted"},
		{"truncated junit", JUnit, string(fixture(t, "pytest.xml")[:300]), "unreadable JUnit file"},
		{"junit without cases", JUnit, `<testsuites><testsuite name="empty" tests="0"/></testsuites>`, "no test results"},
		{"junit suite error", JUnit, `<testsuite name="broken" tests="0" errors="1"><system-err>SyntaxError: Unexpected token</system-err></testsuite>`, "SyntaxError: Unexpected token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, err := Parse(tc.format, []byte(tc.data))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(run.Error, tc.errorHas) {
				t.Errorf("run error = %q, want it to hold %q", run.Error, tc.errorHas)
			}
		})
	}
	if _, err := Parse("tap", nil); err == nil {
		t.Error("unknown format parsed")
	}
}

func TestParseFiles(t *testing.T) {
	files := map[string][]byte{
		"TEST-com.example.shop.CalcTest.xml": fixture(t, "TEST-com.example.shop.CalcTest.xml"),
		"TEST-com.example.shop.CartTest.xml": fixture(t, "TEST-com.example.shop.CartTest.xml"),
	}
	run, err := ParseFiles(JUnit, files)
	if err != nil {
		t.Fatal(err)
	}
	if run.Error != "" || len(run.Results) != 6 {
		t.Fatalf("run = %+v", run)
	}
	if got := run.ByID()["com.example.shop.CartTest::totals"]; got.Status != Passed || got.Seconds != 1204.5 {
		t.Errorf("totals = %+v", got)
	}

	files["TEST-com.example.shop.OrderTest.xml"] = nil
	run, _ = ParseFiles(JUnit, files)
	if !strings.Contains(run.Error, "TEST-com.example.shop.OrderTest.xml: unreadable JUnit file") || len(run.Results) != 6 {
		t.Errorf("a missing file must crash the run and keep the others: %q, %d results", run.Error, len(run.Results))
	}

	run, _ = ParseFiles(JUnit, nil)
	if run.Error == "" {
		t.Error("no files must crash the run")
	}
}

func run(error string, results ...Result) Run { return Run{Results: results, Error: error} }

func res(id string, s Status) Result { return Result{ID: id, Status: s} }

func TestFailToPass(t *testing.T) {
	base := []Run{run("", res("a", Failed), res("b", Error), res("c", Passed), res("d", Skipped))}
	merged := run("", res("a", Passed), res("b", Passed), res("c", Passed), res("d", Passed), res("new", Passed), res("e", Failed))
	if got, want := FailToPass(base, []Run{merged, merged, merged}), []string{"a", "b", "d", "new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("FailToPass = %v, want %v", got, want)
	}

	// A base build failure means the tests are absent, so they count.
	crashedBase := []Run{run("pkg: build failed")}
	if got, want := FailToPass(crashedBase, []Run{merged}), []string{"a", "b", "c", "d", "new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("FailToPass over a crashed base = %v, want %v", got, want)
	}

	// Passing in one base run is enough to disqualify.
	twoBase := []Run{run("", res("a", Failed)), run("", res("a", Passed))}
	if got := FailToPass(twoBase, []Run{merged}); slices.Contains(got, "a") {
		t.Errorf("a passed at base but is fail-to-pass: %v", got)
	}

	// It must pass in every merged run; a crashed merged run passes nothing.
	flaky := run("", res("a", Failed), res("b", Passed))
	if got, want := FailToPass(base, []Run{merged, flaky, merged}), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("FailToPass with a flaky run = %v, want %v", got, want)
	}
	if got := FailToPass(base, []Run{merged, run("panic in TestMain", res("a", Passed))}); got != nil {
		t.Errorf("FailToPass with a crashed merged run = %v", got)
	}
	if got := FailToPass(base, nil); got != nil {
		t.Errorf("FailToPass with no merged runs = %v", got)
	}
}

func TestPassToPass(t *testing.T) {
	base := run("", res("p/a", Passed), res("p/b", Passed), res("q/c", Passed), res("p/d", Failed), res("p/e", Passed), res("p/f", Skipped))
	merged := run("", res("p/a", Passed), res("p/b", Passed), res("q/c", Passed), res("p/d", Passed), res("p/e", Failed), res("p/f", Passed))
	inP := func(id string) bool { return strings.HasPrefix(id, "p/") }
	if got, want := PassToPass(base, []Run{merged, merged, merged}, inP, 200), []string{"p/a", "p/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PassToPass = %v, want %v", got, want)
	}
	if got, want := PassToPass(base, []Run{merged}, nil, 2), []string{"p/a", "p/b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("capped PassToPass = %v, want %v", got, want)
	}
	if got, want := PassToPass(base, []Run{merged}, nil, 0), []string{"p/a", "p/b", "q/c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("uncapped PassToPass = %v, want %v", got, want)
	}
	missing := run("", res("p/a", Passed))
	if got, want := PassToPass(base, []Run{merged, missing}, inP, 200), []string{"p/a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PassToPass with a test absent from one run = %v, want %v", got, want)
	}
	if got := PassToPass(base, []Run{merged, run("killed", res("p/a", Passed))}, inP, 200); got != nil {
		t.Errorf("PassToPass with a crashed merged run = %v", got)
	}
}

func TestFlaky(t *testing.T) {
	r1 := run("", res("a", Passed), res("b", Passed), res("c", Passed))
	r2 := run("", res("a", Passed), res("b", Failed), res("c", Passed))
	r3 := run("", res("a", Passed), res("c", Passed))
	if got, want := Flaky([]Run{r1, r2, r3}, []string{"c", "b", "a"}), []string{"b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Flaky = %v, want %v", got, want)
	}
	if got, want := Flaky([]Run{r1, run("timed out", res("a", Passed))}, []string{"a", "c"}), []string{"a", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Flaky with a crashed run = %v, want %v", got, want)
	}
}
