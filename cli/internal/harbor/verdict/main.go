// Command verdict is the verifier program of an exported Harbor task (docs/specs/cases.md, "Harbor
// export"). The export copies this file and the oracle package's source into the task's
// environment, where a build stage compiles them, so the task needs neither Casebox nor Go at
// verification time. It reads the result files of each test command with the oracle's parsers,
// checks every fail-to-pass and pass-to-pass test of tests/oracle.json, and writes reward.txt
// (1 when every one of them passed, else 0) and reward.json with the counts.
//
// Usage: casebox-verdict [-oracle file] [-out dir] [-error text] [format=dir ...]
//
// Each format=dir argument is one test command's results: every regular file under dir, in one
// of the oracle's formats. A command whose run crashed passes none of its tests. With -error,
// no test ran and the reward is 0.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/alternayte/casebox/cli/internal/oracle"
)

type oracleTests struct {
	FailToPass []string `json:"failToPass"`
	PassToPass []string `json:"passToPass"`
}

type reward struct {
	Reward           int `json:"reward"`
	FailToPass       int `json:"failToPass"`
	FailToPassPassed int `json:"failToPassPassed"`
	PassToPass       int `json:"passToPass"`
	PassToPassPassed int `json:"passToPassPassed"`
}

func main() {
	oraclePath := flag.String("oracle", "/tests/oracle.json", "the oracle's test lists")
	out := flag.String("out", "/logs/verifier", "where reward.txt and reward.json go")
	failure := flag.String("error", "", "why no test ran; the reward is 0")
	flag.Parse()
	if err := run(*oraclePath, *out, *failure, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "verdict:", err)
		// A verifier that cannot decide still leaves a reward of 0.
		_ = os.MkdirAll(*out, 0o755)
		_ = os.WriteFile(filepath.Join(*out, "reward.txt"), []byte("0\n"), 0o644)
		os.Exit(1)
	}
}

func run(oraclePath, out, failure string, args []string) error {
	data, err := os.ReadFile(oraclePath)
	if err != nil {
		return err
	}
	var tests oracleTests
	if err := json.Unmarshal(data, &tests); err != nil {
		return fmt.Errorf("read %s: %w", oraclePath, err)
	}
	passed := map[string]bool{}
	if failure != "" {
		fmt.Println("No test ran:", failure)
	} else {
		for _, arg := range args {
			format, dir, ok := strings.Cut(arg, "=")
			if !ok {
				return fmt.Errorf("%q is not format=dir", arg)
			}
			files, err := readTree(dir)
			if err != nil {
				return err
			}
			res, err := oracle.ParseFiles(format, files)
			if err != nil {
				return err
			}
			if res.Error != "" {
				fmt.Printf("The run of %s crashed; none of its tests count:\n%s\n", dir, res.Error)
				continue
			}
			for _, r := range res.Results {
				if r.Status == oracle.Passed {
					passed[r.ID] = true
				} else {
					delete(passed, r.ID)
				}
			}
		}
	}
	r := reward{FailToPass: len(tests.FailToPass), PassToPass: len(tests.PassToPass)}
	r.FailToPassPassed = count(tests.FailToPass, passed, "fail-to-pass")
	r.PassToPassPassed = count(tests.PassToPass, passed, "pass-to-pass")
	if failure == "" && r.FailToPassPassed == r.FailToPass && r.PassToPassPassed == r.PassToPass {
		r.Reward = 1
	}
	fmt.Printf("Fail-to-pass %d of %d passed; pass-to-pass %d of %d passed; reward %d.\n",
		r.FailToPassPassed, r.FailToPass, r.PassToPassPassed, r.PassToPass, r.Reward)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "reward.json"), append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "reward.txt"), []byte(fmt.Sprintf("%d\n", r.Reward)), 0o644)
}

// count returns how many of ids passed and prints the ones that did not.
func count(ids []string, passed map[string]bool, list string) int {
	n := 0
	for _, id := range ids {
		if passed[id] {
			n++
			continue
		}
		fmt.Printf("Not passed (%s): %s\n", list, id)
	}
	return n
}

// readTree reads every regular file under dir, by slash path relative to it. A missing dir is a
// command that left no results, which the parser reports as a crashed run.
func readTree(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
