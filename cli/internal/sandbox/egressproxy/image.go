package egressproxy

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"sort"
)

// source is proxy.go itself: the proxy image compiles it, so the proxy that runs is the one
// this binary was built with.
//
//go:embed proxy.go
var source []byte

// Builder is the image that compiles the proxy, pinned by version and digest. Only the build
// stage uses it; the proxy image is the static binary on scratch.
const Builder = "golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1"

// Port is where the proxy listens in its container.
const Port = "3128"

const mainSource = `package main

import (
	"os"

	egressproxy "casebox.local/egress/proxy"
)

func main() { os.Exit(egressproxy.Main(os.Args[1:])) }
`

const dockerfile = `FROM ` + Builder + ` AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS= go build -trimpath -ldflags=-s -o /egress-proxy .
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /egress-proxy /egress-proxy
USER 65534:65534
ENTRYPOINT ["/egress-proxy"]
`

// BuildContext is a docker build context (path to contents, with a Dockerfile) that compiles the
// proxy into a static binary on scratch, run as nobody, with the builder's CA bundle for the
// https:// requests it makes itself. It needs no network beyond pulling Builder:
// the proxy uses the standard library only.
func BuildContext() map[string][]byte {
	return map[string][]byte{
		"Dockerfile":     []byte(dockerfile),
		"go.mod":         []byte("module casebox.local/egress\n\ngo 1.26\n"),
		"main.go":        []byte(mainSource),
		"proxy/proxy.go": source,
	}
}

// ImageTag names the proxy image by the hash of its build context, so a changed proxy builds a new
// image and an unchanged one is built once per machine.
func ImageTag() string {
	files := BuildContext()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte{0})
		h.Write(files[n])
		h.Write([]byte{0})
	}
	return "casebox-egress:" + hex.EncodeToString(h.Sum(nil))[:24]
}
