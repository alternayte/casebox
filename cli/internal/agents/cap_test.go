package agents

import (
	"bytes"
	"io"
	"testing"
)

// countingReader records how far the watcher read, to show it returned at the crossing.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	// One byte at a time, so the scanner cannot read ahead past the line that crossed the cap.
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestWatchCapStopsClaudeCodeAtTheCrossing(t *testing.T) {
	out := read(t, "claude-code.stdout.jsonl")
	a := adapter(t, ClaudeCode)
	if !a.StreamsUsage() {
		t.Fatal("Claude Code's stream-json streams usage")
	}

	// Cache reads do not count. msg_01 is on two lines with the same usage, 620 tokens, counted
	// once: a cap of 620 is passed only by msg_02 (40 more, 660), although msg_01 alone read 1000
	// tokens from the cache.
	r := &countingReader{r: bytes.NewReader(out)}
	res, err := a.WatchCap(r, 620)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exceeded || Tokens(res.Usage) != 660 || res.Usage.CacheReadTokens != 2500 {
		t.Fatalf("cap 620: %+v, tokens %d", res, Tokens(res.Usage))
	}
	if r.read >= len(out) {
		t.Fatal("the watcher read to the end instead of returning at the crossing")
	}

	// msg_03 (115 more) passes a cap of 660.
	res, err = a.WatchCap(bytes.NewReader(out), 660)
	if err != nil || !res.Exceeded || Tokens(res.Usage) != 775 {
		t.Fatalf("cap 660: %+v, tokens %d, %v", res, Tokens(res.Usage), err)
	}

	res, err = a.WatchCap(bytes.NewReader(out), 1_000_000)
	if err != nil || res.Exceeded || Tokens(res.Usage) != 817 {
		t.Fatalf("no crossing: %+v, tokens %d, %v", res, Tokens(res.Usage), err)
	}
	res, err = a.WatchCap(bytes.NewReader(out), 0)
	if err != nil || res.Exceeded {
		t.Fatalf("no cap: %+v, %v", res, err)
	}
}

// Codex and the Cursor CLI report usage only at the end; the watcher still reads it, so the cap
// is checked after the run from the same numbers.
func TestWatchCapAfterTheRun(t *testing.T) {
	// Codex: 900 uncached input and 280 output (its 4400 cached tokens are cache reads); Cursor:
	// 900 input, 120 output and 200 cache writes (its 3000 cache reads do not count).
	cases := map[string]int64{Codex: 1180, CursorCLI: 1220}
	for agent, tokens := range cases {
		a := adapter(t, agent)
		if a.StreamsUsage() {
			t.Errorf("%s does not stream usage during the run", agent)
		}
		res, err := a.WatchCap(bytes.NewReader(read(t, agent+".stdout.jsonl")), 1200)
		if err != nil {
			t.Fatal(err)
		}
		if Tokens(res.Usage) != tokens || res.Exceeded != (tokens > 1200) {
			t.Errorf("%s: %+v, tokens %d", agent, res, Tokens(res.Usage))
		}
	}
}
