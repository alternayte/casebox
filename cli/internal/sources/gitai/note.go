// Package gitai parses git-ai notes (refs/notes/ai, authorship/3.0.0) into the line ranges an
// agent wrote. Real notes break the standard in small ways (unsorted and adjacent ranges, junk
// keys, missing records), so the parser is lenient. Human authors in a note are never read out.
package gitai

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
)

// Attribution is the lines of one file an agent wrote in one commit.
type Attribution struct {
	Path   string `json:"path"`
	Agent  string `json:"agent,omitempty"`
	Model  string `json:"model,omitempty"`
	Ranges string `json:"ranges"` // "1-24,30-41", sorted and merged
}

type agentID struct {
	Tool  string `json:"tool"`
	Model string `json:"model"`
}

type metadata struct {
	Sessions map[string]struct {
		AgentID agentID `json:"agent_id"`
	} `json:"sessions"`
	Prompts map[string]struct {
		AgentID agentID `json:"agent_id"`
	} `json:"prompts"`
}

// Parse reads one note and returns the agent-written ranges per file and agent.
func Parse(note string) ([]Attribution, error) {
	head, tail, found := strings.Cut(note, "\n---\n")
	if !found {
		if strings.HasPrefix(note, "---\n") {
			head, tail = "", note[4:]
		} else {
			return nil, nil
		}
	}
	var meta metadata
	if err := json.Unmarshal([]byte(tail), &meta); err != nil {
		return nil, err
	}
	agentOf := func(key string) (agentID, bool) {
		if session, _, ok := strings.Cut(key, "::"); ok && strings.HasPrefix(session, "s_") {
			s, found := meta.Sessions[session]
			return s.AgentID, found
		}
		if p, found := meta.Prompts[key]; found {
			return p.AgentID, true
		}
		return agentID{}, false // h_ keys are people, and unknown keys are unresolved
	}

	type group struct {
		path  string
		agent agentID
	}
	lines := map[group]map[int]bool{}
	var order []group
	path := ""
	scanner := bufio.NewScanner(strings.NewReader(head))
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			path = strings.Trim(line, `"`)
			continue
		}
		key, ranges, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || path == "" {
			continue
		}
		agent, isAgent := agentOf(key)
		if !isAgent {
			continue
		}
		g := group{path, agent}
		if lines[g] == nil {
			lines[g] = map[int]bool{}
			order = append(order, g)
		}
		for _, r := range strings.Split(ranges, ",") {
			lo, hi, isRange := strings.Cut(strings.TrimSpace(r), "-")
			a, err1 := strconv.Atoi(lo)
			b := a
			var err2 error
			if isRange {
				b, err2 = strconv.Atoi(hi)
			}
			if err1 != nil || err2 != nil || a < 1 || b < a || b-a > 1_000_000 {
				continue
			}
			for n := a; n <= b; n++ {
				lines[g][n] = true
			}
		}
	}
	out := make([]Attribution, 0, len(order))
	for _, g := range order {
		if r := compact(lines[g]); r != "" {
			out = append(out, Attribution{Path: g.path, Agent: g.agent.Tool, Model: g.agent.Model, Ranges: r})
		}
	}
	return out, scanner.Err()
}

func compact(set map[int]bool) string {
	if len(set) == 0 {
		return ""
	}
	max := 0
	for n := range set {
		if n > max {
			max = n
		}
	}
	var parts []string
	for n := 1; n <= max; n++ {
		if !set[n] {
			continue
		}
		start := n
		for set[n+1] {
			n++
		}
		if start == n {
			parts = append(parts, strconv.Itoa(n))
		} else {
			parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(n))
		}
	}
	return strings.Join(parts, ",")
}
