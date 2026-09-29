using System.Text;
using Amazon.Runtime;
using Amazon.S3;
using Casebox.Server.Features.Blobs;
using Casebox.Server.Tests.Infrastructure;
using Npgsql;

namespace Casebox.Server.Tests.Features;

// The S3-compatible store against RustFS: the bytes live in the bucket, the index row in Postgres.
public sealed class S3BlobStoreTests(StackFixture stack)
{
    [Fact]
    public async Task A_blob_round_trips_through_an_S3_compatible_bucket()
    {
        var ct = TestContext.Current.CancellationToken;
        using var s3 = new AmazonS3Client(
            new BasicAWSCredentials(StackFixture.S3AccessKey, StackFixture.S3SecretKey),
            new AmazonS3Config { ServiceURL = stack.S3Url, ForcePathStyle = true, AuthenticationRegion = "us-east-1" });
        await s3.PutBucketAsync(StackFixture.Bucket, ct);

        await using var dataSource = NpgsqlDataSource.Create(stack.ConnectionString);
        var store = new S3BlobStore(dataSource, TimeProvider.System, s3, StackFixture.Bucket);
        var data = Encoding.UTF8.GetBytes($"s3 blob {Guid.NewGuid()}");
        var hash = BlobStore.HashOf(data);

        Assert.True(await store.PutAsync(StackFixture.OrgA, hash, "text/plain", data, ct));
        Assert.False(await store.PutAsync(StackFixture.OrgA, hash, "text/plain", data, ct));
        var blob = await store.GetAsync(StackFixture.OrgA, hash, ct);
        Assert.Equal(data, blob!.Data);
        Assert.Null(await store.GetAsync(StackFixture.OrgB, hash, ct));

        var objects = await s3.ListObjectsV2Async(new() { BucketName = StackFixture.Bucket, Prefix = $"{StackFixture.OrgA}/" }, ct);
        Assert.Contains(objects.S3Objects, o => o.Key.EndsWith($"{hash}.zst", StringComparison.Ordinal));
    }
}
