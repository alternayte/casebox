# Build context: the repository root.
FROM oven/bun:1 AS web
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

FROM mcr.microsoft.com/dotnet/sdk:10.0 AS build
WORKDIR /src
COPY global.json ./
COPY server/ server/
COPY --from=web /src/server/Casebox.Server/wwwroot server/Casebox.Server/wwwroot
RUN dotnet publish server/Casebox.Server/Casebox.Server.csproj -c Release -o /app

FROM mcr.microsoft.com/dotnet/aspnet:10.0
LABEL org.opencontainers.image.source="https://github.com/alternayte/casebox" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.description="Casebox server: API, web UI, OTLP ingest and the job queue"
# Npgsql probes for Kerberos at start-up and logs an error when the library is missing.
RUN apt-get update && apt-get install -y --no-install-recommends libgssapi-krb5-2 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /app ./
# Numeric, so Kubernetes can verify runAsNonRoot.
USER $APP_UID
ENTRYPOINT ["dotnet", "Casebox.Server.dll"]
