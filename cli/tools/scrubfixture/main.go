// Command scrubfixture turns a real agent transcript into a parser fixture: it keeps the
// structure, IDs, timestamps, enum values and the marker texts parsers rely on, and replaces
// every other string with a placeholder. Review the output before committing it.
//
//	go run ./tools/scrubfixture -lines 400 < transcript.jsonl > fixture.jsonl
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Keys whose string values are structure, not content.
var keep = map[string]bool{
	"type": true, "subtype": true, "role": true, "stop_reason": true, "model": true, "toolDenialKind": true,
	"kind": true, "reason": true, "status": true, "trigger": true, "phase": true, "history_mode": true,
	"cli_version": true, "version": true, "userType": true, "entrypoint": true, "permissionMode": true,
	"timestamp": true, "uuid": true, "parentUuid": true, "logicalParentUuid": true, "sessionId": true,
	"session_id": true, "id": true, "call_id": true, "tool_use_id": true, "turn_id": true, "requestId": true,
	"promptId": true, "level": true, "originator": true, "effort": true, "operation": true, "mode": true,
	"thread_source": true, "item_type": true, "hook_event_name": true, "leafUuid": true, "agentId": true,
	"messageId": true, "interruptedMessageId": true, "stop_details": true, "service_tier": true, "speed": true,
}

// Tool names are structure, but only in a tool call's name field.
var toolNameParents = map[string]bool{"tool_use": true, "function_call": true, "custom_tool_call": true, "local_shell_call": true}

// Texts parsers match on; a string that starts with one keeps that prefix.
var markers = []string{
	"[Request interrupted by user for tool use]",
	"[Request interrupted by user]",
	"The user doesn't want to proceed with this tool use.",
	"Permission for this command was denied",
	"exec command rejected by user",
	"patch rejected by user",
	"<turn_aborted>",
}

var fixed = map[string]string{"cwd": "/work/app", "gitBranch": "main", "branch": "main", "repository_url": "git@github.com:acme/app.git", "commit_hash": "0123456789abcdef0123456789abcdef01234567"}

func main() {
	lines := flag.Int("lines", 0, "keep only the first n lines (0 keeps all)")
	flag.Parse()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for n := 0; in.Scan(); n++ {
		if *lines > 0 && n >= *lines {
			break
		}
		var v any
		if err := json.Unmarshal(in.Bytes(), &v); err != nil {
			fmt.Fprintln(os.Stderr, "skip a line that is not JSON")
			continue
		}
		data, _ := json.Marshal(scrub("", v, ""))
		out.Write(data)
		out.WriteByte('\n')
	}
}

func scrub(key string, v any, parentType string) any {
	switch x := v.(type) {
	case map[string]any:
		t, _ := x["type"].(string)
		out := make(map[string]any, len(x))
		for k, child := range x {
			// Some formats key objects by file path; a path is content.
			if strings.ContainsAny(k, "/\\") {
				sum := sha256.Sum256([]byte(k))
				out["path-"+hex.EncodeToString(sum[:4])] = scrub(k, child, t)
				continue
			}
			out[k] = scrub(k, child, t)
		}
		return out
	case []any:
		for i, child := range x {
			x[i] = scrub(key, child, parentType)
		}
		return x
	case string:
		switch {
		case fixed[key] != "":
			return fixed[key]
		case keep[key], key == "name" && toolNameParents[parentType]:
			return x
		}
		for _, m := range markers {
			if strings.HasPrefix(strings.TrimSpace(x), m) {
				return m
			}
		}
		if x == "" {
			return x
		}
		sum := sha256.Sum256([]byte(x))
		return "text-" + hex.EncodeToString(sum[:4])
	default:
		return v
	}
}
