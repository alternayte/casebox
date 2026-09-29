package kiln

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKiln serves exactly the Kiln endpoints the provider calls, with the request and response
// shapes of Kiln v0.7.0 (internal/api and sdk/openapi.json). Request bodies are decoded the way
// Kiln decodes them: unknown fields are refused, and required fields are checked.
type fakeKiln struct {
	t     *testing.T
	token string

	mu sync.Mutex
	// buildPolls is how many reads a new build stays in building; buildError fails it.
	buildPolls int
	buildError string
	templates  map[string]*fakeTemplate
	sandboxes  map[string]*fakeSandbox
	snapshots  map[string]map[string][]byte
	// run answers a provider command: its argv script, env and timeout, with the stdin bytes it
	// redirects from. It returns stdout, stderr and the result Kiln reports.
	run func(req execRequest, stdin []byte) (stdout, stderr []byte, res execResponse)

	templatePosts []createTemplateRequest
	sandboxPosts  []createSandboxRequest
	execs         []execRequest
	snapshotPosts []snapshotRequest
	restorePosts  []restoreRequest
	next          int
}

type fakeTemplate struct {
	req   createTemplateRequest
	state string
	polls int
	err   string
}

type fakeSandbox struct {
	id        string
	template  string
	req       createSandboxRequest
	destroyed *time.Time
	files     map[string][]byte
}

// The request shapes of Kiln's internal/api.
type createTemplateRequest struct {
	Name        string    `json:"name"`
	Image       string    `json:"image"`
	VCPUs       int       `json:"vcpus"`
	MemoryMB    int       `json:"memory_mb"`
	DiskMB      int       `json:"disk_mb"`
	Setup       []string  `json:"setup"`
	EgressAllow *[]string `json:"egress_allow"`
	TTLSeconds  *int      `json:"ttl_seconds"`
	Start       []string  `json:"start"`
	Port        int       `json:"port"`
}

type createSandboxRequest struct {
	Template    string            `json:"template"`
	Lifecycle   string            `json:"lifecycle"`
	IdleSeconds *int              `json:"idle_seconds"`
	TTLSeconds  *int              `json:"ttl_seconds"`
	Metadata    json.RawMessage   `json:"metadata"`
	Secrets     []string          `json:"secrets"`
	Env         map[string]string `json:"env"`
}

type execRequest struct {
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type execResponse struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type snapshotRequest struct {
	Stop bool `json:"stop"`
}

type restoreRequest struct {
	Count           int  `json:"count"`
	AllowSecretFork bool `json:"allow_secret_fork"`
}

func newFake(t *testing.T) (*fakeKiln, *Provider) {
	t.Helper()
	f := &fakeKiln{
		t:          t,
		token:      "kiln-test-key",
		buildPolls: 2,
		templates:  map[string]*fakeTemplate{},
		sandboxes:  map[string]*fakeSandbox{},
		snapshots:  map[string]map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/templates", f.createTemplate)
	mux.HandleFunc("GET /v1/templates/{name}", f.getTemplate)
	mux.HandleFunc("POST /v1/sandboxes", f.createSandbox)
	mux.HandleFunc("GET /v1/sandboxes/{id}", f.getSandbox)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", f.deleteSandbox)
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", f.exec)
	mux.HandleFunc("GET /v1/sandboxes/{id}/files/{path...}", f.readFile)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files/{path...}", f.writeFile)
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", f.snapshot)
	mux.HandleFunc("POST /v1/snapshots/{id}/restore", f.restore)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "the credential is missing")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	p := New(srv.URL, f.token)
	p.poll = time.Millisecond
	return f, p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return false
	}
	return true
}

func (f *fakeKiln) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s%04d", prefix, f.next)
}

func (f *fakeKiln) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req createTemplateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.EgressAllow == nil {
		writeError(w, http.StatusBadRequest, "invalid", "egress_allow is required and may be empty")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.templatePosts = append(f.templatePosts, req)
	if t, ok := f.templates[req.Name]; ok && t.state != "failed" {
		writeError(w, http.StatusConflict, "conflict", "template "+req.Name+" is "+t.state)
		return
	}
	f.templates[req.Name] = &fakeTemplate{req: req, state: "building", polls: f.buildPolls}
	writeJSON(w, http.StatusAccepted, map[string]string{"name": req.Name, "state": "building"})
}

func (f *fakeKiln) getTemplate(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.templates[r.PathValue("name")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return
	}
	if t.state == "building" {
		if t.polls > 0 {
			t.polls--
		} else if f.buildError != "" {
			t.state, t.err = "failed", f.buildError
		} else {
			t.state = "ready"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": t.req.Name, "image": t.req.Image, "vcpus": t.req.VCPUs, "memory_mb": t.req.MemoryMB,
		"disk_mb": t.req.DiskMB, "egress_allow": *t.req.EgressAllow, "state": t.state, "error": t.err,
		"snapshot_bytes": 0, "sandboxes": 0, "created_at": time.Now().UTC(),
	})
}

func (f *fakeKiln) sandboxJSON(sb *fakeSandbox) map[string]any {
	state := "running"
	if sb.destroyed != nil {
		state = "destroyed"
	}
	out := map[string]any{
		"id": sb.id, "state": state, "template": sb.template, "lifecycle": sb.req.Lifecycle,
		"idle_seconds": sb.req.IdleSeconds, "ttl_seconds": sb.req.TTLSeconds, "metadata": json.RawMessage(`{}`),
		"created_at": time.Now().UTC(), "last_active_at": time.Now().UTC(), "published": []any{}, "env_keys": []string{},
	}
	if sb.destroyed != nil {
		out["destroyed_at"] = sb.destroyed
	}
	return out
}

func (f *fakeKiln) createSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxRequest
	if !decode(w, r, &req) {
		return
	}
	if req.IdleSeconds == nil {
		writeError(w, http.StatusBadRequest, "invalid", "idle_seconds is required")
		return
	}
	if req.Lifecycle != "ephemeral" && req.Lifecycle != "persistent" {
		writeError(w, http.StatusBadRequest, "invalid", `lifecycle must be "ephemeral" or "persistent"`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sandboxPosts = append(f.sandboxPosts, req)
	t, ok := f.templates[req.Template]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return
	}
	if t.state != "ready" {
		writeError(w, http.StatusConflict, "conflict", "store: conflict")
		return
	}
	sb := &fakeSandbox{id: f.id("sb"), template: req.Template, req: req, files: map[string][]byte{}}
	f.sandboxes[sb.id] = sb
	writeJSON(w, http.StatusCreated, f.sandboxJSON(sb))
}

// live returns a running sandbox, or answers as Kiln does: 404 for an unknown id and 409 for a
// destroyed one.
func (f *fakeKiln) live(w http.ResponseWriter, id string) (*fakeSandbox, bool) {
	sb, ok := f.sandboxes[id]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return nil, false
	}
	if sb.destroyed != nil {
		writeError(w, http.StatusConflict, "conflict", "store: conflict")
		return nil, false
	}
	return sb, true
}

func (f *fakeKiln) getSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return
	}
	writeJSON(w, http.StatusOK, f.sandboxJSON(sb))
}

func (f *fakeKiln) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return
	}
	if sb.destroyed == nil {
		now := time.Now().UTC()
		sb.destroyed = &now
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	redirects = regexp.MustCompile(`<'([^']*)' >'([^']*)' 2>'([^']*)'$`)
	tarCmd    = regexp.MustCompile(`tar -c -f '([^']*)' -C '([^']*)' '([^']*)'$`)
)

func (f *fakeKiln) exec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "invalid", "cmd is required")
		return
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 3600 {
		writeError(w, http.StatusBadRequest, "invalid", "timeout_seconds must be between 1 and 3600")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.live(w, r.PathValue("id"))
	if !ok {
		return
	}
	f.execs = append(f.execs, req)
	if req.Cmd[0] == "rm" {
		for name := range sb.files {
			if strings.HasPrefix(name, req.Cmd[2]+"/") {
				delete(sb.files, name)
			}
		}
		writeJSON(w, http.StatusOK, execResponse{})
		return
	}
	script := req.Cmd[len(req.Cmd)-1]
	if m := tarCmd.FindStringSubmatch(script); m != nil {
		archive, err := f.tar(sb, m[2], m[3])
		if err != nil {
			writeJSON(w, http.StatusOK, execResponse{ExitCode: 1, Stderr: "tar: " + err.Error() + "\n"})
			return
		}
		sb.files[m[1]] = archive
		writeJSON(w, http.StatusOK, execResponse{})
		return
	}
	m := redirects.FindStringSubmatch(script)
	if m == nil {
		f.t.Errorf("the fake cannot read the command %q", script)
		writeError(w, http.StatusBadRequest, "invalid", "unknown command")
		return
	}
	var stdin []byte
	if m[1] != "/dev/null" {
		data, ok := sb.files[m[1]]
		if !ok {
			writeJSON(w, http.StatusOK, execResponse{ExitCode: 1, Stderr: "sh: can't open " + m[1] + "\n"})
			return
		}
		stdin = data
	}
	stdout, stderr, res := f.run(req, stdin)
	if res.ExitCode == 125 {
		// The wrapper exits 125 when it cannot make its directory, before any redirect.
		writeJSON(w, http.StatusOK, res)
		return
	}
	sb.files[m[2]] = stdout
	sb.files[m[3]] = stderr
	writeJSON(w, http.StatusOK, res)
}

// tar makes the archive tar -c -C parent base would: base and everything under it.
func (f *fakeKiln) tar(sb *fakeSandbox, parent, base string) ([]byte, error) {
	root := strings.TrimSuffix(parent, "/") + "/" + base
	var names []string
	for name := range sb.files {
		if name == root || strings.HasPrefix(name, root+"/") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, errors.New(base + ": No such file or directory")
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		body := sb.files[name]
		rel := strings.TrimPrefix(strings.TrimPrefix(name, strings.TrimSuffix(parent, "/")), "/")
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (f *fakeKiln) readFile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.live(w, r.PathValue("id"))
	if !ok {
		return
	}
	data, ok := sb.files["/"+r.PathValue("path")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "open: no such file or directory")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (f *fakeKiln) writeFile(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.live(w, r.PathValue("id"))
	if !ok {
		return
	}
	sb.files["/"+r.PathValue("path")] = body
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeKiln) snapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotRequest
	if !decode(w, r, &req) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.live(w, r.PathValue("id"))
	if !ok {
		return
	}
	f.snapshotPosts = append(f.snapshotPosts, req)
	id := f.id("snap")
	files := make(map[string][]byte, len(sb.files))
	for k, v := range sb.files {
		files[k] = v
	}
	f.snapshots[id] = files
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot_id": id, "size_bytes": 4096})
}

func (f *fakeKiln) restore(w http.ResponseWriter, r *http.Request) {
	var req restoreRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Count < 1 {
		writeError(w, http.StatusBadRequest, "invalid", "count must be greater than zero")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restorePosts = append(f.restorePosts, req)
	files, ok := f.snapshots[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "store: not found")
		return
	}
	var refs []map[string]string
	for range req.Count {
		sb := &fakeSandbox{id: f.id("sb"), req: createSandboxRequest{Lifecycle: "ephemeral"}, files: map[string][]byte{}}
		for k, v := range files {
			sb.files[k] = v
		}
		f.sandboxes[sb.id] = sb
		refs = append(refs, map[string]string{"id": sb.id, "state": "running"})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sandboxes": refs})
}
