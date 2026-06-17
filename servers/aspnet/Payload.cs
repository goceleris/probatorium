using System.Text;

namespace AspnetAdapter;

// Deterministic JSON payload generator — must match servers/common/payload.go
// byte for byte so the loadgen fixtures compare equal across every adapter
// (verified with cmp against the live common.Endpoints bytes: /json-1k =
// 1026 bytes, /json-8k = 8286 bytes, /json-16k = 16463 bytes,
// /json-64k = 65618 bytes).
//
// The Go reference builds a paginated envelope
//   {"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[ ...items... ]}
// appending one item per iteration (id is 1-based) and re-checking the
// encoded length after every append; it returns the full buffer the first
// time that length reaches the target. No truncation — the final item is
// always complete. We hand-emit the same bytes here (no serializer) so key
// order and separators are fixed and identical to encoding/json's output
// for the reference struct.
internal static class Payload
{
    internal static readonly byte[] Json1k = Generate(1024);

    // Mid-size payloads bridge the 1k→64k gap: on the 20G LACP fabric the
    // 64k cells are NIC-bound (fast adapters converge at line rate), so the
    // 8k/16k cells stay under the ceiling and keep differentiating adapters
    // by CPU throughput. Same parametric generator as the Go reference
    // (generateJSONPayload with targets 8192 / 16384).
    internal static readonly byte[] Json8k = Generate(8192);
    internal static readonly byte[] Json16k = Generate(16384);

    internal static readonly byte[] Json64k = Generate(65536);

    private static byte[] Generate(int target)
    {
        const string header = "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":[";
        const string footer = "]}";

        var b = new StringBuilder(target + 256);
        b.Append(header);

        for (var i = 1; ; i++)
        {
            if (i > 1)
            {
                b.Append(',');
            }

            AppendItem(b, i);

            // Mirror encoding/json: the closing "]}" is part of the final
            // length check. The Go reference returns as soon as the full
            // marshalled response (with footer) reaches target.
            if (b.Length + footer.Length >= target)
            {
                break;
            }
        }

        b.Append(footer);
        return Encoding.UTF8.GetBytes(b.ToString());
    }

    private static void AppendItem(StringBuilder b, int n)
    {
        b.Append("{\"id\":").Append(n)
            .Append(",\"name\":\"User ").Append(n)
            .Append("\",\"email\":\"user").Append(n)
            .Append("@example.com\",\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}");
    }
}
