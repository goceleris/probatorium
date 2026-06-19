package io.goceleris.probatorium.netty;

import java.nio.charset.StandardCharsets;

/**
 * Deterministic JSON payload generator — must match
 * probatorium/servers/common/payload.go byte for byte so the loadgen
 * fixtures compare equal across every adapter. Verified sizes against the
 * live common.Endpoints bytes: /json-1k = 1026, /json-8k = 8286,
 * /json-16k = 16463, /json-64k = 65618.
 *
 * <p>The Go reference (encoding/json over a struct) emits the envelope
 *
 * <pre>{"page":1,"per_page":50,"total":1000,"total_pages":20,"data":[ ...items... ]}</pre>
 *
 * appending one item per iteration (1-based id) and re-marshalling the full
 * response after each append; it returns the buffer the first time the
 * marshalled length (footer included) reaches the target. We hand-emit the
 * same bytes here (no serializer) so key order and separators are fixed and
 * identical to encoding/json's output for the reference struct.
 */
final class Payload {
    static final byte[] JSON_1K = generate(1024);

    // Mid-size payloads bridge the 1k->64k gap: on the 20G fabric the 64k
    // cells are NIC-bound (fast adapters converge at line rate), so the
    // 8k/16k cells stay under the ceiling and keep differentiating adapters
    // by CPU throughput. Same parametric generator as the Go reference.
    static final byte[] JSON_8K = generate(8192);
    static final byte[] JSON_16K = generate(16384);
    static final byte[] JSON_64K = generate(65536);

    private Payload() {}

    private static byte[] generate(int target) {
        final String header =
                "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":[";
        final String footer = "]}";

        StringBuilder b = new StringBuilder(target + 256);
        b.append(header);

        for (int i = 1; ; i++) {
            if (i > 1) {
                b.append(',');
            }
            appendItem(b, i);

            // Mirror encoding/json: the closing "]}" is part of the final
            // length check. The Go reference returns as soon as the full
            // marshalled response (with footer) reaches target.
            if (b.length() + footer.length() >= target) {
                break;
            }
        }

        b.append(footer);
        return b.toString().getBytes(StandardCharsets.UTF_8);
    }

    private static void appendItem(StringBuilder b, int n) {
        b.append("{\"id\":").append(n)
                .append(",\"name\":\"User ").append(n)
                .append("\",\"email\":\"user").append(n)
                .append("@example.com\",\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}");
    }
}
