// Deterministic 1 KiB / 64 KiB JSON payload generator.
//
// Byte-identical port of probatorium/servers/common/payload.go. The Go
// reference marshals the (paginatedResponse, paginatedItem) struct pair
// with encoding/json, which emits compact JSON in struct-declaration field
// order: page, per_page, total, total_pages, data for the wrapper; id,
// name, email, status, created_at for each item.
//
// We emit the bytes by hand rather than via a JSON encoder: byte-for-byte
// equivalence with the Go reference is a hard conformance requirement
// (the runner's assertContract does an exact bytes compare), and the
// corpus is tiny pure-ASCII, so manual formatting is both correct and
// trivially auditable. The termination rule mirrors the Go loop — append
// items until the marshalled length crosses targetSize — fixing the sizes:
//   1 KiB target  -> 1026 bytes ending at item 9
//   64 KiB target -> 65618 bytes ending at item 583

const std = @import("std");

const header = "{\"page\":1,\"per_page\":50,\"total\":1000,\"total_pages\":20,\"data\":[";
const footer = "]}";

// generate builds a paginated-response payload of at least target_size
// bytes into a freshly allocated buffer owned by the caller.
pub fn generate(alloc: std.mem.Allocator, target_size: usize) ![]u8 {
    var buf: std.ArrayList(u8) = .empty;
    errdefer buf.deinit(alloc);

    try buf.appendSlice(alloc, header);
    var i: u64 = 1;
    while (true) : (i += 1) {
        if (i > 1) try buf.append(alloc, ',');
        try appendItem(alloc, &buf, i);
        // Tentative size = current buf + footer length; the footer is
        // fixed-length so we never need to re-marshal the whole thing.
        if (buf.items.len + footer.len >= target_size) break;
    }
    try buf.appendSlice(alloc, footer);
    return buf.toOwnedSlice(alloc);
}

// appendItem writes one paginatedItem in the exact byte form
// encoding/json produces for the Go struct:
//   {"id":<n>,"name":"User <n>","email":"user<n>@example.com",
//    "status":"active","created_at":"2024-01-15T09:30:00Z"}
fn appendItem(alloc: std.mem.Allocator, buf: *std.ArrayList(u8), n: u64) !void {
    var num: [20]u8 = undefined;
    const ns = std.fmt.bufPrint(&num, "{d}", .{n}) catch unreachable;

    try buf.appendSlice(alloc, "{\"id\":");
    try buf.appendSlice(alloc, ns);
    try buf.appendSlice(alloc, ",\"name\":\"User ");
    try buf.appendSlice(alloc, ns);
    try buf.appendSlice(alloc, "\",\"email\":\"user");
    try buf.appendSlice(alloc, ns);
    try buf.appendSlice(alloc, "@example.com\",\"status\":\"active\",\"created_at\":\"2024-01-15T09:30:00Z\"}");
}
