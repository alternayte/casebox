// Package skill holds Casebox's agent skill, which `casebox skill install` writes for Claude Code
// and Cursor.
package skill

import _ "embed"

// Text is the skill: front matter with its name and description, then the instructions.
//
//go:embed SKILL.md
var Text string
