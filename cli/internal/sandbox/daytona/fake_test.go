package daytona

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake is Daytona's platform API, toolbox proxy and object storage, plus an image registry,
// with the request and response shapes of Daytona's OpenAPI documents. It records what the
// provider sent. Commands do not run; a test's onExecute decides what a command did.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	nextID    int
	snapshots map[string]*fakeSnapshot // by name
	sandboxes map[string]*fakeSandbox  // by ID
	files     map[string]map[string][]byte
	objects   map[string][]byte // object storage key to body
	s3Headers http.Header
	calls     []string // "METHOD path" of every platform and toolbox request
	executes  []fakeExec

	// buildResult is the state a snapshot reaches after building; default active.
	buildResult func(name string) (state, reason string)
	// requireStop makes a sandbox snapshot answer 400 unless the sandbox is stopped.
	requireStop bool
	// onExecute decides what a command did. It runs with mu held; the default removes the
	// files an "rm -f" names and succeeds.
	onExecute func(sandboxID, command string, files map[string][]byte) (int, executeResponse)
}

type fakeSnapshot struct {
	dto   snapshotDTO
	req   createSnapshot
	steps []string
}

type fakeSandbox struct {
	dto   sandboxDTO
	req   createSandbox
	next  string // the state the next GET moves to
	after string // the state a snapshot returns to
}

type fakeExec struct {
	Sandbox string
	Command string
	Timeout int
}

func newFake(t *testing.T) (*fake, *Provider) {
	t.Helper()
	f := &fake{t: t, snapshots: map[string]*fakeSnapshot{}, sandboxes: map[string]*fakeSandbox{}, files: map[string]map[string][]byte{}, objects: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	p := New(f.srv.URL+"/api", "key")
	p.poll = time.Millisecond
	p.registry = func(string) string { return f.srv.URL }
	return f, p
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/api/"):
		f.calls = append(f.calls, r.Method+" "+strings.TrimPrefix(path, "/api"))
		if r.Header.Get("Authorization") != "Bearer key" {
			problem(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		f.platform(w, r, strings.TrimPrefix(path, "/api"))
	case strings.HasPrefix(path, "/toolbox/"):
		f.calls = append(f.calls, r.Method+" "+path)
		if r.Header.Get("Authorization") != "Bearer key" {
			problem(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		f.toolbox(w, r, strings.TrimPrefix(path, "/toolbox/"))
	case strings.HasPrefix(path, "/s3/"):
		body, _ := io.ReadAll(r.Body)
		f.objects[strings.TrimPrefix(path, "/s3/")] = body
		f.s3Headers = r.Header.Clone()
	case strings.HasPrefix(path, "/v2/") || path == "/token":
		f.registry(w, r)
	default:
		http.NotFound(w, r)
	}
}

func problem(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"statusCode": status, "message": message, "error": http.StatusText(status)})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) decode(r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		f.t.Errorf("%s %s: the body is not JSON: %v", r.Method, r.URL.Path, err)
	}
}

func (f *fake) platform(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "/object-storage/push-access":
		reply(w, storageAccess{AccessKey: "AK", Secret: "SK", SessionToken: "ST", StorageURL: f.srv.URL + "/s3", OrganizationID: "org1", Bucket: "builds"})

	case r.Method == http.MethodPost && path == "/snapshots":
		var req createSnapshot
		f.decode(r, &req)
		if _, ok := f.snapshots[req.Name]; ok {
			problem(w, http.StatusConflict, fmt.Sprintf("Snapshot with name %q already exists for this organization", req.Name))
			return
		}
		state, reason := "active", ""
		if f.buildResult != nil {
			state, reason = f.buildResult(req.Name)
		}
		f.nextID++
		s := &fakeSnapshot{dto: snapshotDTO{ID: fmt.Sprintf("snap-%d", f.nextID), Name: req.Name, State: "pending"}, req: req, steps: []string{"building", state}}
		s.dto.ErrorReason = reason
		f.snapshots[req.Name] = s
		reply(w, s.dto)
	case len(parts) == 2 && parts[0] == "snapshots" && r.Method == http.MethodGet:
		s := f.snapshotByIDOrName(parts[1])
		if s == nil {
			problem(w, http.StatusNotFound, "Snapshot "+parts[1]+" not found")
			return
		}
		if len(s.steps) > 0 {
			s.dto.State, s.steps = s.steps[0], s.steps[1:]
		}
		reply(w, s.dto)
	case len(parts) == 2 && parts[0] == "snapshots" && r.Method == http.MethodDelete:
		s := f.snapshotByIDOrName(parts[1])
		if s == nil {
			problem(w, http.StatusNotFound, "Snapshot not found")
			return
		}
		delete(f.snapshots, s.dto.Name)

	case r.Method == http.MethodPost && path == "/sandbox":
		var req createSandbox
		f.decode(r, &req)
		if _, ok := f.snapshots[req.Snapshot]; !ok {
			problem(w, http.StatusBadRequest, "Snapshot "+req.Snapshot+" not found. Did you add it through the Daytona Dashboard?")
			return
		}
		if req.LinkedSandbox != "" && (req.AutoDeleteInterval == nil || *req.AutoDeleteInterval != 0) {
			problem(w, http.StatusBadRequest, "Linked sandboxes must be ephemeral (set autoDeleteInterval to 0)")
			return
		}
		f.nextID++
		id := fmt.Sprintf("sb-%d", f.nextID)
		f.sandboxes[id] = &fakeSandbox{dto: sandboxDTO{ID: id, State: "creating", CPU: 1, Memory: 1, ToolboxProxyURL: f.srv.URL + "/toolbox"}, req: req, next: "started"}
		f.files[id] = map[string][]byte{}
		reply(w, f.sandboxes[id].dto)
	case len(parts) >= 2 && parts[0] == "sandbox":
		s := f.sandboxes[parts[1]]
		if s == nil {
			problem(w, http.StatusNotFound, "Sandbox with ID or name "+parts[1]+" not found")
			return
		}
		action := ""
		if len(parts) == 3 {
			action = parts[2]
		}
		f.sandboxAction(w, r, s, action)
	default:
		problem(w, http.StatusNotFound, "Cannot "+r.Method+" "+path)
	}
}

func (f *fake) snapshotByIDOrName(key string) *fakeSnapshot {
	if s, ok := f.snapshots[key]; ok {
		return s
	}
	for _, s := range f.snapshots {
		if s.dto.ID == key {
			return s
		}
	}
	return nil
}

func (f *fake) sandboxAction(w http.ResponseWriter, r *http.Request, s *fakeSandbox, action string) {
	id := s.dto.ID
	switch {
	case action == "" && r.Method == http.MethodGet:
		if s.dto.State == "destroying" {
			delete(f.sandboxes, id)
			problem(w, http.StatusNotFound, "Sandbox with ID or name "+id+" not found")
			return
		}
		if s.next != "" {
			s.dto.State, s.next = s.next, ""
		} else if s.after != "" {
			s.dto.State, s.after = s.after, ""
		}
		reply(w, s.dto)
	case action == "" && r.Method == http.MethodDelete:
		s.dto.State = "destroying"
		for _, child := range f.sandboxes {
			if child.req.LinkedSandbox == id {
				child.dto.State = "destroying"
			}
		}
		reply(w, s.dto)
	case action == "resize" && r.Method == http.MethodPost:
		var req resizeSandbox
		f.decode(r, &req)
		s.dto.CPU, s.dto.Memory = float64(req.CPU), float64(req.Memory)
		s.dto.State, s.next = "resizing", "started"
		reply(w, s.dto)
	case action == "stop" && r.Method == http.MethodPost:
		s.dto.State, s.next = "stopping", "stopped"
		reply(w, s.dto)
	case action == "start" && r.Method == http.MethodPost:
		s.dto.State, s.next = "starting", "started"
		reply(w, s.dto)
	case action == "snapshot" && r.Method == http.MethodPost:
		var req sandboxSnapshot
		f.decode(r, &req)
		if f.requireStop && s.dto.State != "stopped" {
			problem(w, http.StatusBadRequest, "Filesystem-only snapshots require the sandbox to be stopped (STOPPED)")
			return
		}
		f.nextID++
		f.snapshots[req.Name] = &fakeSnapshot{dto: snapshotDTO{ID: fmt.Sprintf("snap-%d", f.nextID), Name: req.Name, State: "snapshotting"}, steps: []string{"active"}}
		s.after = s.dto.State
		s.dto.State = "snapshotting"
		reply(w, s.dto)
	default:
		problem(w, http.StatusNotFound, "Cannot "+r.Method+" "+r.URL.Path)
	}
}

func (f *fake) toolbox(w http.ResponseWriter, r *http.Request, rest string) {
	id, endpoint, _ := strings.Cut(rest, "/")
	s := f.sandboxes[id]
	if s == nil || s.dto.State == "destroying" {
		problem(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if s.dto.State != "started" {
		problem(w, http.StatusBadRequest, "sandbox is not running")
		return
	}
	files := f.files[id]
	switch endpoint {
	case "files/upload-v2":
		body, _ := io.ReadAll(r.Body)
		files[r.URL.Query().Get("path")] = body
		reply(w, map[string]string{"name": "f", "path": r.URL.Query().Get("path"), "type": "file"})
	case "files/download":
		body, ok := files[r.URL.Query().Get("path")]
		if !ok {
			problem(w, http.StatusNotFound, "file not found")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	case "process/execute":
		var req executeRequest
		f.decode(r, &req)
		f.executes = append(f.executes, fakeExec{Sandbox: id, Command: req.Command, Timeout: req.Timeout})
		if strings.HasPrefix(req.Command, "rm -f ") {
			for _, word := range strings.Fields(strings.TrimPrefix(req.Command, "rm -f ")) {
				delete(files, strings.Trim(word, "'"))
			}
			reply(w, executeResponse{ExitCode: 0})
			return
		}
		status, res := http.StatusOK, executeResponse{}
		if f.onExecute != nil {
			status, res = f.onExecute(id, req.Command, files)
		}
		if status != http.StatusOK {
			problem(w, status, "command execution timeout")
			return
		}
		reply(w, res)
	default:
		problem(w, http.StatusNotFound, "no toolbox endpoint "+endpoint)
	}
}

// registry serves one public repository, "cache", behind an anonymous bearer token.
func (f *fake) registry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		if r.URL.Query().Get("scope") != "repository:cache:pull" || r.URL.Query().Get("service") != "registry.test" {
			problem(w, http.StatusBadRequest, "bad scope "+r.URL.RawQuery)
			return
		}
		reply(w, map[string]string{"token": "pull-token"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer pull-token" {
		w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:cache:pull"`, f.srv.URL))
		problem(w, http.StatusUnauthorized, "authentication required")
		return
	}
	switch r.URL.Path {
	case "/v2/cache/manifests/7":
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			problem(w, http.StatusBadRequest, "no index accepted")
			return
		}
		reply(w, map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "manifests": []map[string]any{
			{"digest": "sha256:arm", "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
			{"digest": "sha256:amd", "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		}})
	case "/v2/cache/manifests/sha256:amd":
		reply(w, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "config": map[string]string{"digest": "sha256:config"}})
	case "/v2/cache/blobs/sha256:config":
		reply(w, map[string]any{"config": map[string]any{"Entrypoint": []string{"docker-entrypoint.sh"}, "Cmd": []string{"redis-server"}}})
	default:
		problem(w, http.StatusNotFound, "manifest unknown")
	}
}

// count returns how many recorded calls are exactly call.
func (f *fake) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// created returns the create request of every sandbox, in creation order.
func (f *fake) created() []createSandbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]createSandbox, 0, len(f.sandboxes))
	for i := 1; i <= f.nextID; i++ {
		if s, ok := f.sandboxes[fmt.Sprintf("sb-%d", i)]; ok {
			out = append(out, s.req)
		}
	}
	return out
}

// commands returns the executed commands of one sandbox.
func (f *fake) commands(id string) []fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeExec
	for _, e := range f.executes {
		if e.Sandbox == id {
			out = append(out, e)
		}
	}
	return out
}
