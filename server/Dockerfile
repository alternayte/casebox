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
# Npgsql probes for Kerberos at start-up and logs an error when the library is missing.
RUN apt-get update && apt-get install -y --no-install-recommends libgssapi-krb5-2 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /app ./
USER app
ENTRYPOINT ["dotnet", "Casebox.Server.dll"]
