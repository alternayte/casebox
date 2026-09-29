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

// Error is a non-2xx answer. Title is the problem title the server sent.
type Error struct {
	Status int
	Title  string
}

func (e *Error) Error() string {
	if e.Title != "" {
		return fmt.Sprintf("the server answered %d: %s", e.Status, e.Title)
	}
	return fmt.Sprintf("the server answered %d", e.Status)
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(data, &problem)
		return &Error{Status: resp.StatusCode, Title: problem.Title}
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
		return "", fmt.Errorf("reach %s: %w", c.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var problem struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&problem)
		return "", &Error{Status: resp.StatusCode, Title: problem.Title}
	}
	return hash, nil
}
