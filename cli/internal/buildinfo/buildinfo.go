// Package buildinfo holds the version stamped into the binary at link time.
package buildinfo

// Version is set with -ldflags "-X github.com/alternayte/casebox/cli/internal/buildinfo.Version=…".
var Version = "0.2.0-dev"
