package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Link is the work item that sessions in one repository belong to, set by casebox link.
type Link struct {
	WorkItem string    `json:"workItem"`
	At       time.Time `json:"at"`
}

func linkPath(repoRoot string) (string, error) {
	sum := sha256.Sum256([]byte(repoRoot))
	return Path("links", hex.EncodeToString(sum[:8])+".json")
}

// SetLink records the work item for the repository at repoRoot. An empty work item clears it.
func SetLink(repoRoot, workItem string) error {
	path, err := linkPath(repoRoot)
	if err != nil {
		return err
	}
	if workItem == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(Link{WorkItem: workItem, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600)
}

// CurrentLink returns the repository's work item: CASEBOX_WORK_ITEM first, then casebox link.
func CurrentLink(repoRoot string) string {
	if item := os.Getenv("CASEBOX_WORK_ITEM"); item != "" {
		return item
	}
	path, err := linkPath(repoRoot)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var l Link
	if json.Unmarshal(data, &l) != nil {
		return ""
	}
	return l.WorkItem
}
