package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/alternayte/casebox/cli/internal/api"
	"github.com/alternayte/casebox/cli/internal/config"
)

// serverConfig is the server's capture settings, cached so a hook never waits on the network.
type serverConfig struct {
	PromptMode     *string   `json:"promptMode"`
	CaptureAllowed bool      `json:"captureAllowed"`
	FetchedAt      time.Time `json:"fetchedAt"`
}

// Mode returns the prompt mode from the cache. Without a fresh cache it returns off, so no text
// is kept that the server's mode might not allow.
func Mode() string {
	path, err := config.Path("cache", "server-config.json")
	if err != nil {
		return ModeOff
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ModeOff
	}
	var c serverConfig
	if json.Unmarshal(data, &c) != nil || c.PromptMode == nil || time.Since(c.FetchedAt) > 7*24*time.Hour {
		return ModeOff
	}
	return *c.PromptMode
}

// RefreshMode reads the prompt mode from the server and caches it.
func RefreshMode(ctx context.Context, client *api.Client) (string, error) {
	var c serverConfig
	if err := client.Do(ctx, http.MethodGet, "/ingest/v1/config", nil, &c); err != nil {
		return "", err
	}
	c.FetchedAt = time.Now().UTC()
	path, err := config.Path("cache", "server-config.json")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	if c.PromptMode == nil {
		return "", nil
	}
	return *c.PromptMode, nil
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
