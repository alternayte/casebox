package repo

import (
	"fmt"
	"sort"
)

// Price is one model's price in USD per million tokens (SDD section 13, prices). Cache read and
// cache write are optional; without them the server prices cached tokens as input.
type Price struct {
	Input      *float64 `yaml:"input" json:"input"`
	Output     *float64 `yaml:"output" json:"output"`
	CacheRead  *float64 `yaml:"cache_read,omitempty" json:"cacheRead,omitempty"`
	CacheWrite *float64 `yaml:"cache_write,omitempty" json:"cacheWrite,omitempty"`
}

// Evaluation is the evaluation block of casebox.yml: the baseline casebox compare starts from.
type Evaluation struct {
	Baseline *Baseline `yaml:"baseline"`
}

// Baseline names the agent, its pinned version, the model and the settings of the baseline side.
// Its harness is the repository's default branch unless casebox compare names another.
type Baseline struct {
	Agent          string        `yaml:"agent"`
	AgentVersion   string        `yaml:"agent_version"`
	Model          string        `yaml:"model"`
	Effort         string        `yaml:"effort,omitempty"`
	MaxTurns       *int          `yaml:"max_turns,omitempty"`
	TimeoutMinutes *int          `yaml:"timeout_minutes,omitempty"`
	TokenCap       *int64        `yaml:"token_cap,omitempty"`
	Command        *CommandAgent `yaml:"command,omitempty"`
}

// CommandAgent is the template of the command agent: any headless CLI. {instruction_file} and
// {model} are replaced; log_glob finds its session log and log_format names the parser.
type CommandAgent struct {
	Template  string `yaml:"template"`
	LogGlob   string `yaml:"log_glob,omitempty"`
	LogFormat string `yaml:"log_format,omitempty"`
}

// PriceTable returns the prices of casebox.yml, or says which entry is incomplete or negative.
func (c Config) PriceTable() (map[string]Price, error) {
	models := make([]string, 0, len(c.Prices))
	for m := range c.Prices {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, m := range models {
		p := c.Prices[m]
		if p.Input == nil || p.Output == nil {
			return nil, fmt.Errorf("casebox.yml prices.%s needs input and output (USD per million tokens)", m)
		}
		for name, v := range map[string]*float64{"input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_write": p.CacheWrite} {
			if v != nil && *v < 0 {
				return nil, fmt.Errorf("casebox.yml prices.%s.%s is negative", m, name)
			}
		}
	}
	return c.Prices, nil
}
