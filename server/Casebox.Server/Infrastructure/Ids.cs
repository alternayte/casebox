namespace Casebox.Server;

// IDs are ULIDs: sortable by creation time, safe in URLs and stream IDs.
public static class Ids
{
    public static string New() => Ulid.NewUlid().ToString();
}
