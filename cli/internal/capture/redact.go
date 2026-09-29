package capture

import (
	"os"
	"regexp"
	"strings"
)

// A redaction rule. The server repeats the same rules (server/Casebox.Server/Features/Capture/Redaction.cs);
// checks/redaction-rules.sh keeps the two lists the same.
type rule struct {
	name    string
	pattern *regexp.Regexp
	replace string
}

var rules = []rule{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted:private_key]"},
	{"aws_key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), "[redacted:aws_key]"},
	{"github_token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})\b`), "[redacted:github_token]"},
	{"slack_token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`), "[redacted:slack_token]"},
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`), "[redacted:anthropic_key]"},
	{"openai_key", regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{20,}\b`), "[redacted:openai_key]"},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), "[redacted:jwt]"},
	{"bearer", regexp.MustCompile(`(\b[Bb]earer\s+)[A-Za-z0-9._~+/=-]{16,}`), "${1}[redacted:bearer]"},
	{"url_credentials", regexp.MustCompile(`(\b[a-z][a-z0-9+.-]*://)[^\s/:@]+:[^\s/@]+@`), "${1}[redacted:url_credentials]@"},
	// Go's regexp has no look-ahead, so a value that is already redacted is skipped in the replacer.
	{"assignment", regexp.MustCompile(`(?i)(\b[\w.-]*(?:password|passwd|secret|token|key)[\w.-]*)(["']?\s*[:=]\s*["']?)([^\s"',;]{4,})`), ""},
}

var homePath = regexp.MustCompile(`(/Users/|/home/|[A-Z]:\\Users\\)[^/\\\s"']+`)

// Redactor removes secrets and scrubs machine details. Team rules come from casebox.yml
// (capture.redact) and apply after the built-in rules.
type Redactor struct {
	team []*regexp.Regexp
	home string
	host *regexp.Regexp
}

// NewRedactor builds a redactor for this machine with the team's extra patterns.
func NewRedactor(team []string) (*Redactor, error) {
	r := &Redactor{}
	for _, p := range team {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, err
		}
		r.team = append(r.team, re)
	}
	r.home, _ = os.UserHomeDir()
	if host, err := os.Hostname(); err == nil && len(host) >= 3 {
		r.host = regexp.MustCompile(`\b` + regexp.QuoteMeta(host) + `\b`)
	}
	return r, nil
}

// Redact returns text with secrets replaced by [redacted:<rule>], the home directory by ~ and
// the host name by host.
func (r *Redactor) Redact(text string) string {
	if text == "" {
		return text
	}
	for _, rl := range rules {
		if rl.name == "assignment" {
			text = rl.pattern.ReplaceAllStringFunc(text, func(m string) string {
				parts := rl.pattern.FindStringSubmatch(m)
				if strings.HasPrefix(parts[3], "[redacted") {
					return m
				}
				return parts[1] + parts[2] + "[redacted:assignment]"
			})
			continue
		}
		text = rl.pattern.ReplaceAllString(text, rl.replace)
	}
	for _, re := range r.team {
		text = re.ReplaceAllString(text, "[redacted:team]")
	}
	if r.home != "" {
		text = strings.ReplaceAll(text, r.home, "~")
	}
	text = homePath.ReplaceAllString(text, "~")
	if r.host != nil {
		text = r.host.ReplaceAllString(text, "host")
	}
	return text
}
