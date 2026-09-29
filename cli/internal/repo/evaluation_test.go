package repo

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestTheEvaluationBlockAndPricesReadFromCaseboxYml(t *testing.T) {
	var c Config
	err := yaml.Unmarshal([]byte(`
evaluation:
  baseline: { agent: claude-code, agent_version: 2.4.1, model: claude-sonnet-5, effort: high, max_turns: 200, timeout_minutes: 30, token_cap: 2000000 }
prices:
  claude-sonnet-5: { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 }
  gpt-6: { input: 1.25, output: 10 }
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	b := c.Evaluation.Baseline
	if b == nil || b.Agent != "claude-code" || b.AgentVersion != "2.4.1" || b.Model != "claude-sonnet-5" || b.Effort != "high" ||
		*b.MaxTurns != 200 || *b.TimeoutMinutes != 30 || *b.TokenCap != 2_000_000 {
		t.Fatalf("baseline = %+v", b)
	}
	prices, err := c.PriceTable()
	if err != nil {
		t.Fatal(err)
	}
	if p := prices["claude-sonnet-5"]; *p.Input != 3 || *p.Output != 15 || *p.CacheRead != 0.3 || *p.CacheWrite != 3.75 {
		t.Fatalf("claude-sonnet-5 = %+v", p)
	}
	if p := prices["gpt-6"]; p.CacheRead != nil || p.CacheWrite != nil {
		t.Fatalf("gpt-6 has cache prices it does not name: %+v", p)
	}
}

func TestAPriceWithoutOutputIsRefused(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("prices:\n  m-1: { input: 3 }\n"), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PriceTable(); err == nil || !strings.Contains(err.Error(), "prices.m-1 needs input and output") {
		t.Fatalf("err = %v", err)
	}
}
