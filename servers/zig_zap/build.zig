// Build script for the zig_zap probatorium adapter.
//
// Produces a single ReleaseFast executable named `zig_zap` under
// zig-out/bin/. The conformance test and the cluster build invoke
// `zig build -Doptimize=ReleaseFast`; the runner then execs the binary
// with `-bind <addr>` and waits for the `ready addr=` line on stdout.
//
// Dependency-free: the adapter is built on std.http.Server (see the NOTE
// in src/main.zig on why zigzap/zap is not used under Zig 0.16), so there
// is no package graph to resolve.

const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    // Leave preferred_optimize_mode unset so the standard `-Doptimize` flag is
    // registered (passing a preferred mode instead registers `-Drelease`). The
    // cluster build and the conformance test both invoke
    // `zig build -Doptimize=ReleaseFast`.
    const optimize = b.standardOptimizeOption(.{});

    const exe = b.addExecutable(.{
        .name = "zig_zap",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/main.zig"),
            .target = target,
            .optimize = optimize,
        }),
    });

    b.installArtifact(exe);
}
