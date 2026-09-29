# The worker image (docs/specs/operations.md, Images). Build context: the repository root.
FROM golang:1.26-alpine AS build
WORKDIR /src/cli
COPY cli/go.mod cli/go.sum ./
RUN go mod download
COPY cli/ ./
ARG VERSION=0.1.0-dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/alternayte/casebox/cli/internal/buildinfo.Version=${VERSION}" -o /out/casebox ./cmd/casebox

# git for the worker's mirrors.
FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/alternayte/casebox" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="Casebox worker: steering classification and proposal drafts"
RUN apk add --no-cache ca-certificates git \
    && adduser -D -u 10001 casebox
COPY --from=build /out/casebox /usr/local/bin/casebox
USER 10001
ENV CASEBOX_HOME=/home/casebox/.casebox
ENTRYPOINT ["casebox", "worker"]
