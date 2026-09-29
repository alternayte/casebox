// Package api is the CLI's client for the Casebox server's REST API.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alternayte/casebox/cli/internal/cbx"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one server with one token.
type Client struct {
	Server string
	Token  string
	HTTP   *http.Client
}

// New returns a client with a 30-second timeout.
func New(server, token string) *Client {
	return &Client{Server: strings.TrimRight(server, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is a non-2xx answer. Title is the problem title the server sent, Code its CBX code.
type Error struct {
	Status int
	Title  string
	Code   string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("the server answered %d", e.Status)
	if e.Title != "" {
		msg = fmt.Sprintf("the server answered %d: %s", e.Status, e.Title)
	}
	if e.Code != "" {
		msg += fmt.Sprintf(" (%s: %s)", e.Code, cbx.URL(e.Code))
	}
	return msg
}

// codeOf is the problem's code, or the code its status implies.
func codeOf(status int, code string) string {
	switch {
	case code != "":
		return code
	case status == http.StatusUnauthorized:
		return cbx.Unauthenticated
	case status == http.StatusForbidden:
		return cbx.Forbidden
	}
	return ""
}

// StatusOf returns the HTTP status of an *Error, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Do sends body as JSON and decodes a JSON answer into out, when out is not nil.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	return c.DoWith(ctx, method, path, nil, body, out)
}

// DoWith is Do with extra request headers.
func (c *Client) DoWith(ctx context.Context, method, path string, headers map[string]string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return cbx.Errorf(cbx.Unreachable, "reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Title string `json:"title"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(data, &problem)
		return &Error{Status: resp.StatusCode, Title: problem.Title, Code: codeOf(resp.StatusCode, problem.Code)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// PutBlob uploads content-addressed bytes (worker and ingest tokens) and returns their hash.
func (c *Client) PutBlob(ctx context.Context, contentType string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Server+"/worker/v1/blobs/"+hash, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", cbx.Errorf(cbx.Unreachable, "reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Title string `json:"title"`
			Code  string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&problem)
		return "", &Error{Status: resp.StatusCode, Title: problem.Title, Code: codeOf(resp.StatusCode, problem.Code)}
	}
	return hash, nil
}
