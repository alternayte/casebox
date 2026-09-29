namespace Casebox.Server.Tests.Infrastructure;

// The images every server test runs against. deploy/compose.yaml pins the same QueueBox version;
// checks/queuebox-pin.sh fails when the two differ.
internal static class Images
{
    public const string Postgres = "postgres:16-alpine";
    public const string QueueBox = "ghcr.io/alternayte/queuebox:0.6.0";
    public const string Oidc = "ghcr.io/navikt/mock-oauth2-server:6.0.3";
    public const string S3 = "rustfs/rustfs:1.0.0";
}
