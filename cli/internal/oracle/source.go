package oracle

import "embed"

// Source is this package's Go source. The Harbor export compiles it, with its _test.go files
// left out, into each task's verifier, so a Harbor verdict reads results by the same rules.
//
//go:embed *.go
var Source embed.FS
