package probatorium.vertx;

import io.vertx.core.buffer.Buffer;
import java.nio.charset.StandardCharsets;

/**
 * Deterministic 1k / 8k / 16k / 64k JSON payload generator.
 *
 * <p>Byte-identical port of {@code probatorium/servers/common/payload.go}. The
 * Go reference marshals a {@code (paginatedResponse, paginatedItem)} struct pair
 * with {@code encoding/json}, which emits compact JSON in struct-declaration
 * field order: {@code page, per_page, total, total_pages, data} for the wrapper
 * and {@code id, name, email, status, created_at} per item.
 *
 * <p>We emit the bytes by hand rather than via a JSON library: byte-for-byte
 * equivalence with the Go reference is a hard conformance requirement
 * (cmd/conformance does a bytes-equal compare) and the corpus is tiny, pure
 * ASCII, and generated once at startup — so the manual path is both correct and
 * trivially auditable, with no risk of a Jackson/JSON-B formatting drift.
 *
 * <p>The Go termination rule is "append items until the marshalled length
 * crosses targetSize"; we mirror it exactly. Resulting sizes match the Go
 * reference and every other adapter:
 * <pre>
 *   1k  -> 1026  bytes   8k -> 8286  bytes
 *   16k -> 16463 bytes  64k -> 65618 bytes
 * </pre>
 */
final class Payload {

  static final Buffer JSON_1K = generate(1024);
  static final Buffer JSON_8K = generate(8192);
  static final Buffer JSON_16K = generate(16384);
  static final Buffer JSON_64K = generate(65536);

  private Payload() {}

  /** Builds a paginated-response payload of at least {@code targetSize} bytes. */
  private static Buffer generate(int targetSize) {
    // Constant prefix + footer — identical for every payload size.
    byte[] header =
        "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":["
            .getBytes(StandardCharsets.US_ASCII);
    byte[] footer = "]}".getBytes(StandardCharsets.US_ASCII);

    StringBuilder sb = new StringBuilder(targetSize + 256);
    sb.append(new String(header, StandardCharsets.US_ASCII));

    long i = 1;
    while (true) {
      if (i > 1) {
        sb.append(',');
      }
      appendItem(sb, i);
      // Tentative size = current length + footer. Whole corpus is ASCII so
      // String length == byte length; this matches Go's "marshal then measure"
      // predicate without re-encoding on every iteration.
      if (sb.length() + footer.length >= targetSize) {
        break;
      }
      i++;
    }
    sb.append("]}");
    return Buffer.buffer(sb.toString().getBytes(StandardCharsets.US_ASCII));
  }

  /**
   * Writes one paginatedItem in the exact byte form encoding/json produces:
   * {@code {"id":<n>,"name":"User <n>","email":"user<n>@example.com",
   * "status":"active","created_at":"2024-01-15T09:30:00Z"}}.
   */
  private static void appendItem(StringBuilder sb, long n) {
    sb.append("{\"id\":").append(n)
        .append(",\"name\":\"User ").append(n)
        .append("\",\"email\":\"user").append(n)
        .append("@example.com\",\"status\":\"active\",")
        .append("\"created_at\":\"2024-01-15T09:30:00Z\"}");
  }
}
