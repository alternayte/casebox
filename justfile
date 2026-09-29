set windows-shell := ["bash", "-cu"]

# The gate for humans, agents and CI: repo checks, then CLI, web, docs and server.
check: checks cli web docs server

# Repo checks: every script in checks/.
checks:
    @for c in checks/*.sh; do bash "$c" || { echo "FAIL $c" >&2; exit 1; }; done

# Copy the local stack files into the CLI, which embeds them for `casebox up`, and the chart.
sync-stack:
    cp deploy/compose.yaml deploy/queuebox.yml cli/internal/stack/assets/
    cp deploy/queuebox.yml deploy/helm/casebox/files/queuebox.yml

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

# Lint the docs and the README with Vale (the pinned version, downloaded into artifacts/tools).
vale:
    bash docs/vale.sh

# Build the docs site into docs/dist; the build fails on a broken link.
docs: vale
    cd docs && bun install --frozen-lockfile
    cd docs && bun run build

# The docs' screenshots from the demo data, into docs/public/screenshots (and docs/dist when built).
screenshots:
    bash docs/screenshots.sh

# Server: CSharpier's formatting (its defaults, the version pinned in dotnet-tools.json), build, then
# every test on Postgres 16 and the pinned QueueBox image (Testcontainers).
server:
    dotnet tool restore
    dotnet csharpier check server
    dotnet build server/Casebox.slnx -c Release
    dotnet test server/Casebox.slnx -c Release --no-build

