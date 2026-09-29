set windows-shell := ["bash", "-cu"]

# The gate for humans, agents and CI: repo checks, then CLI, web and server.
check: checks cli web server

# Repo checks: every script in checks/.
checks:
    @for c in checks/*.sh; do bash "$c" || { echo "FAIL $c" >&2; exit 1; }; done

# Copy the local stack files into the CLI, which embeds them for `casebox up`.
sync-stack:
    cp deploy/compose.yaml deploy/queuebox.yml cli/internal/stack/assets/

# CLI and worker: format, vet, test, build.
cli:
    cd cli && test -z "$(gofmt -l .)" || { gofmt -l . >&2; echo "gofmt: files above need formatting" >&2; exit 1; }
    cd cli && go vet ./...
    cd cli && go test ./...
    cd cli && go build -o dist/ ./cmd/casebox

# Web UI: typecheck and build into the server's static files.
web:
    cd web && bun install --frozen-lockfile
    cd web && bun run build

# Server: CSharpier's formatting (its defaults, the version pinned in dotnet-tools.json), build, then
# every test on Postgres 16 and the pinned QueueBox image (Testcontainers).
server:
    dotnet tool restore
    dotnet csharpier check server
    dotnet build server/Casebox.slnx -c Release
    dotnet test server/Casebox.slnx -c Release --no-build

