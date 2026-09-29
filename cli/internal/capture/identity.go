package capture

import (
	"regexp"
	"sort"
	"strings"
)

// Mark wraps an identity for the server to tokenize: ⟦cbx:<kind>:<value>⟧. The CLI never
// computes tokens, because the pseudonym secret stays on the server.
func Mark(kind, value string) string {
	return "⟦cbx:" + kind + ":" + value + "⟧"
}

var (
	email = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	// An @mention: after a space or the start, not a decorator call or an attribute access.
	mention  = regexp.MustCompile(`(^|[\s(\[,])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))([^A-Za-z0-9(.-]|$)`)
	existing = regexp.MustCompile(`⟦cbx:[a-z]+:[^⟧]*⟧`)
)

// Marker marks identity spans: emails, @mentions, and the author names in the repository's
// own git log.
type Marker struct {
	names *regexp.Regexp
}

// NewMarker builds a marker for the given author names (from git log). Longer names match first.
func NewMarker(authorNames []string) *Marker {
	var quoted []string
	seen := map[string]bool{}
	for _, n := range authorNames {
		n = strings.TrimSpace(n)
		if len(n) < 3 || seen[n] || strings.Contains(n, "@") {
			continue
		}
		seen[n] = true
		quoted = append(quoted, regexp.QuoteMeta(n))
	}
	sort.Slice(quoted, func(i, j int) bool { return len(quoted[i]) > len(quoted[j]) })
	m := &Marker{}
	if len(quoted) > 0 {
		m.names = regexp.MustCompile(`\b(` + strings.Join(quoted, "|") + `)\b`)
	}
	return m
}

// MarkText marks every identity in text. Spans that are already marked stay as they are.
func (m *Marker) MarkText(text string) string {
	if text == "" {
		return text
	}
	return outsideMarks(text, func(s string) string {
		s = email.ReplaceAllStringFunc(s, func(e string) string { return Mark("email", e) })
		return outsideMarks(s, func(s string) string {
			s = mention.ReplaceAllString(s, "${1}"+Mark("github", "${2}")+"${3}")
			if m.names == nil {
				return s
			}
			return outsideMarks(s, func(s string) string {
				return m.names.ReplaceAllStringFunc(s, func(n string) string { return Mark("name", n) })
			})
		})
	})
}

// outsideMarks applies f to the parts of text that are not already marks.
func outsideMarks(text string, f func(string) string) string {
	locs := existing.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return f(text)
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		b.WriteString(f(text[last:l[0]]))
		b.WriteString(text[l[0]:l[1]])
		last = l[1]
	}
	b.WriteString(f(text[last:]))
	return b.String()
}
