package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// callTimeout bounds one control-plane call. Command execution and file transfers use the
// caller's context instead, because they last as long as the work does.
const callTimeout = 2 * time.Minute

// client talks to one Daytona platform API with one API key. The same key authorises the
// toolbox proxy of every sandbox in the organisation.
type client struct {
	api  string
	key  string
	http *http.Client
}

// apiError is a non-2xx answer from the platform API, the toolbox, object storage or a
// registry. Daytona's platform errors are {statusCode, message, error}; the toolbox's are
// ErrorResponse {statusCode, message, code}.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("daytona answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("daytona answered %d", e.Status)
}

// statusOf returns the HTTP status of an *apiError, or 0.
func statusOf(err error) int {
	var e *apiError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// errorOf reads a non-2xx response into an *apiError.
func errorOf(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &body) == nil {
		var one string
		var many []string
		switch {
		case json.Unmarshal(body.Message, &one) == nil && one != "":
			msg = one
		case json.Unmarshal(body.Message, &many) == nil && len(many) > 0:
			msg = strings.Join(many, "; ")
		default:
			msg = body.Error
		}
	} else {
		msg = strings.TrimSpace(string(data))
	}
	return &apiError{Status: resp.StatusCode, Message: msg}
}

// platform returns the absolute URL of a platform API path.
func (c *client) platform(path string) string {
	return c.api + path
}

// call sends body as JSON to an absolute URL and decodes a JSON answer into out, when out is
// not nil. It bounds the call with callTimeout unless ctx ends sooner.
func (c *client) call(ctx context.Context, method, target string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.do(ctx, method, target, body, out)
}

// do is call without the bound; Exec uses it with the command's own deadline.
func (c *client) do(ctx context.Context, method, target string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach daytona: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorOf(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// upload writes r to path in a sandbox through the toolbox's raw-body upload.
func (c *client) upload(ctx context.Context, toolbox, path string, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, toolbox+"/files/upload-v2?"+url.Values{"path": {path}}.Encode(), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the daytona toolbox: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorOf(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

// download opens a file in a sandbox. The caller closes the body.
func (c *client) download(ctx context.Context, toolbox, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, toolbox+"/files/download?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the daytona toolbox: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, errorOf(resp)
	}
	return resp.Body, nil
}

// Platform API shapes (https://www.daytona.io/docs/openapi.json).

type snapshotDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	ErrorReason string `json:"errorReason"`
}

type createSnapshot struct {
	Name       string     `json:"name"`
	ImageName  string     `json:"imageName,omitempty"`
	Entrypoint []string   `json:"entrypoint,omitempty"`
	BuildInfo  *buildInfo `json:"buildInfo,omitempty"`
}

type buildInfo struct {
	DockerfileContent string   `json:"dockerfileContent"`
	ContextHashes     []string `json:"contextHashes,omitempty"`
}

type sandboxDTO struct {
	ID              string  `json:"id"`
	State           string  `json:"state"`
	ErrorReason     string  `json:"errorReason"`
	CPU             float64 `json:"cpu"`
	Memory          float64 `json:"memory"`
	ToolboxProxyURL string  `json:"toolboxProxyUrl"`
}

type createSandbox struct {
	Snapshot           string            `json:"snapshot"`
	Env                map[string]string `json:"env,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	NetworkBlockAll    bool              `json:"networkBlockAll"`
	AutoStopInterval   *int              `json:"autoStopInterval,omitempty"`
	AutoDeleteInterval *int              `json:"autoDeleteInterval,omitempty"`
	TTLMinutes         *int              `json:"ttlMinutes,omitempty"`
	LinkedSandbox      string            `json:"linkedSandbox,omitempty"`
}

type resizeSandbox struct {
	CPU    int `json:"cpu"`
	Memory int `json:"memory"`
}

type sandboxSnapshot struct {
	Name string `json:"name"`
}

type storageAccess struct {
	AccessKey      string `json:"accessKey"`
	Secret         string `json:"secret"`
	SessionToken   string `json:"sessionToken"`
	StorageURL     string `json:"storageUrl"`
	OrganizationID string `json:"organizationId"`
	Bucket         string `json:"bucket"`
}

// Toolbox API shapes (https://www.daytona.io/docs/toolbox-openapi.json).

type executeRequest struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type executeResponse struct {
	ExitCode int    `json:"exitCode"`
	Result   string `json:"result"`
}

func intp(v int) *int { return &v }
