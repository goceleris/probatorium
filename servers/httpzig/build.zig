// Build script for the httpzig probatorium adapter.
//
// Produces a single ReleaseFast executable named `httpzig` under
// zig-out/bin/. The conformance test and the cluster build invoke
// `zig build -Doptimize=ReleaseFast`; the runner then execs the binary
// with `-bind <addr>` and waits for the `ready addr=` line on stdout.
//
// Depends on karlseguin/http.zig (the `httpz` module), pinned in
// build.zig.zon. `zig build` resolves + fetches it into the build cache
// (ZIG_GLOBAL_CACHE_DIR on the bench host) at build time, so no vendoring.

const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    // Leave preferred_optimize_mode unset so the standard `-Doptimize` flag
    // is registered. The cluster build and the conformance test both invoke
    // `zig build -Doptimize=ReleaseFast`.
    const optimize = b.standardOptimizeOption(.{});

    const httpz = b.dependency("httpz", .{
        .target = target,
        .optimize = optimize,
    });

    const exe = b.addExecutable(.{
        .name = "httpzig",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/main.zig"),
            .target = target,
            .optimize = optimize,
            .imports = &.{
                .{ .name = "httpz", .module = httpz.module("httpz") },
            },
        }),
    });

    b.installArtifact(exe);
}
