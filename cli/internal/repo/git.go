package repo

import (
	"context"
	"fmt"
	"os/exec"
)

// ReadAt reads one file of rev in the repository in dir, a checkout or a bare mirror.
func ReadAt(ctx context.Context, dir, rev, file string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", rev+":"+file)
	cmd.Dir = dir
	return cmd.Output()
}

// Resolve returns the commit a revision names.
func Resolve(ctx context.Context, dir, rev string) (string, error) {
	sha, err := git(ctx, dir, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%s names no commit", rev)
	}
	return sha, nil
}
