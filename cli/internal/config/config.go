// Package config holds the CLI's files under ~/.casebox: credentials, the pause flag and the
// local stack directory. CASEBOX_HOME overrides the location, for tests and CI.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Dir is the CLI's home directory.
func Dir() (string, error) {
	if dir := os.Getenv("CASEBOX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".casebox"), nil
}

// Path joins name to the CLI's home directory and creates the directory.
func Path(name ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(append([]string{dir}, name...)...), nil
}

// Credentials are the server this machine talks to and its tokens.
type Credentials struct {
	Server      string `json:"server"`
	OrgID       string `json:"orgId,omitempty"`
	CLIToken    string `json:"cliToken,omitempty"`
	IngestToken string `json:"ingestToken,omitempty"`
}

// ErrNotLoggedIn means no credentials exist yet.
var ErrNotLoggedIn = errors.New("this machine is not connected to a Casebox server; run casebox init or casebox join")

// LoadCredentials reads ~/.casebox/credentials.json.
func LoadCredentials() (Credentials, error) {
	path, err := Path("credentials.json")
	if err != nil {
		return Credentials{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrNotLoggedIn
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("read %s: %w", path, err)
	}
	return c, nil
}

// SaveCredentials writes ~/.casebox/credentials.json, readable by the owner only.
func SaveCredentials(c Credentials) error {
	path, err := Path("credentials.json")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

// Paused reports whether capture is paused on this machine.
func Paused() bool {
	path, err := Path("paused")
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// SetPaused creates or removes the pause flag.
func SetPaused(paused bool) error {
	path, err := Path("paused")
	if err != nil {
		return err
	}
	if paused {
		return os.WriteFile(path, nil, 0o600)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
