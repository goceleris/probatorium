package corpus

// InitialSeeds is the hand-authored seed corpus. Each entry's Value is
// the master seed handed to the validator's expand pipeline (PCG
// rand source seeded with Value -> workload + fault schedule); Tag is
// a one-line summary of the scenario the seed was designed to exercise.
//
// Order is significant only for human triage; the validator-replay
// harness indexes by Value, not slot. Adding a new seed is append-only
// to keep diffs reviewable; removing a seed requires a CHANGELOG line.
//
// 100 entries land here in wave 6; the corpus grows over time as the
// shrinker mints minimised seeds from incident reports.
var InitialSeeds = []Seed{
	// Baseline (1-7): low-traffic and warmup scenarios — verify the
	// validator pipeline plumbing without exercising any specific bug.
	{Value: 0x1, Tag: "baseline-low-traffic"},
	{Value: 0x2, Tag: "baseline-idle"},
	{Value: 0x3, Tag: "baseline-warmup-only"},
	{Value: 0x4, Tag: "baseline-keep-alive-1conn-1k-reqs"},
	{Value: 0x5, Tag: "baseline-keep-alive-1conn-100k-reqs"},
	{Value: 0x6, Tag: "baseline-no-fault-30m"},
	{Value: 0x7, Tag: "baseline-cookie-only"},

	// HTTP/1.1 adversarial parsing (0x10-0x1c).
	{Value: 0x10, Tag: "h1-pipelining-burst"},
	{Value: 0x11, Tag: "h1-chunked-huge"},
	{Value: 0x12, Tag: "h1-chunked-trailers"},
	{Value: 0x13, Tag: "h1-chunked-zero-no-final-crlf"},
	{Value: 0x14, Tag: "h1-folded-header-rfc-3500"},
	{Value: 0x15, Tag: "h1-content-length-and-transfer-encoding"},
	{Value: 0x16, Tag: "h1-oversize-header-line"},
	{Value: 0x17, Tag: "h1-oversize-uri"},
	{Value: 0x18, Tag: "h1-malformed-request-line"},
	{Value: 0x19, Tag: "h1-bare-cr-in-header"},
	{Value: 0x1a, Tag: "h1-bare-lf-in-header"},
	{Value: 0x1b, Tag: "h1-host-mismatch-vs-absolute-uri"},
	{Value: 0x1c, Tag: "h1-100-continue-burst"},

	// H2C upgrade churn (0x100-0x10b).
	{Value: 0x100, Tag: "h2c-upgrade-during-pause"},
	{Value: 0x101, Tag: "h2c-upgrade-preface-mismatch"},
	{Value: 0x102, Tag: "h2c-upgrade-burst-1k"},
	{Value: 0x103, Tag: "h2c-upgrade-with-body"},
	{Value: 0x104, Tag: "h2c-upgrade-followed-by-rst"},
	{Value: 0x105, Tag: "h2c-noupg-with-h1-request"},
	{Value: 0x106, Tag: "h2c-rapid-reset-cve-2023-44487"},
	{Value: 0x107, Tag: "h2c-settings-flood"},
	{Value: 0x108, Tag: "h2c-ping-flood"},
	{Value: 0x109, Tag: "h2c-continuation-frame-only"},
	{Value: 0x10a, Tag: "h2c-data-after-end-stream"},
	{Value: 0x10b, Tag: "h2c-stream-id-zero-on-data"},

	// WebSocket frame torture (0x200-0x20b).
	{Value: 0x200, Tag: "ws-fragmented-text-many-continuations"},
	{Value: 0x201, Tag: "ws-binary-then-text-without-fin"},
	{Value: 0x202, Tag: "ws-ping-during-fragmented-message"},
	{Value: 0x203, Tag: "ws-close-with-utf8-violation"},
	{Value: 0x204, Tag: "ws-mask-bit-clear-on-client-frame"},
	{Value: 0x205, Tag: "ws-mask-bit-set-on-server-frame"},
	{Value: 0x206, Tag: "ws-rsv-bits-set-no-extension"},
	{Value: 0x207, Tag: "ws-opcode-3-reserved"},
	{Value: 0x208, Tag: "ws-control-frame-fragmented"},
	{Value: 0x209, Tag: "ws-control-frame-oversize"},
	{Value: 0x20a, Tag: "ws-utf8-truncated-continuation"},
	{Value: 0x20b, Tag: "ws-permessage-deflate-bomb-1m"},

	// SSE long-poll (0x300-0x30b).
	{Value: 0x300, Tag: "sse-slow-subscriber-cap-test"},
	{Value: 0x301, Tag: "sse-many-subscribers-1k"},
	{Value: 0x302, Tag: "sse-subscriber-disconnect-mid-event"},
	{Value: 0x303, Tag: "sse-last-event-id-replay"},
	{Value: 0x304, Tag: "sse-retry-jitter"},
	{Value: 0x305, Tag: "sse-comment-only-keepalive"},
	{Value: 0x306, Tag: "sse-event-with-utf8-bom"},
	{Value: 0x307, Tag: "sse-multiline-data-folding"},
	{Value: 0x308, Tag: "sse-broker-fanout-cap-1k"},
	{Value: 0x309, Tag: "sse-broker-fanout-cap-10k"},
	{Value: 0x30a, Tag: "sse-id-field-empty"},
	{Value: 0x30b, Tag: "sse-event-name-with-colon"},

	// Driver consistency (0x400-0x40f).
	{Value: 0x400, Tag: "pg-write-then-read-1-conn"},
	{Value: 0x401, Tag: "pg-write-then-read-100-conns"},
	{Value: 0x402, Tag: "pg-write-then-bounce-pool"},
	{Value: 0x403, Tag: "redis-pipelined-write-read"},
	{Value: 0x404, Tag: "redis-cluster-resharding-during-traffic"},
	{Value: 0x405, Tag: "mc-write-then-read-1-conn"},
	{Value: 0x406, Tag: "mc-binary-protocol-quiet-ops"},
	{Value: 0x407, Tag: "pg-tx-abort-mid-write"},
	{Value: 0x408, Tag: "redis-eval-script-loaded-after-flush"},
	{Value: 0x409, Tag: "pg-listen-notify-burst"},
	{Value: 0x40a, Tag: "pg-pool-exhausted-then-recover"},
	{Value: 0x40b, Tag: "redis-pubsub-subscriber-during-publish-storm"},
	{Value: 0x40c, Tag: "mc-key-eviction-pressure"},
	{Value: 0x40d, Tag: "pg-prepared-stmt-cache-thrash"},
	{Value: 0x40e, Tag: "redis-cluster-slot-migration"},
	{Value: 0x40f, Tag: "mixed-driver-pg-and-redis-same-request"},

	// Fault injection (0x500-0x50f).
	{Value: 0x500, Tag: "fault-tc-delay-100ms"},
	{Value: 0x501, Tag: "fault-tc-delay-jitter-50pct"},
	{Value: 0x502, Tag: "fault-tc-loss-5pct"},
	{Value: 0x503, Tag: "fault-tc-loss-25pct"},
	{Value: 0x504, Tag: "fault-tc-reorder-10pct"},
	{Value: 0x505, Tag: "fault-sigstop-1s-during-load"},
	{Value: 0x506, Tag: "fault-fd-pressure-soft-limit-100"},
	{Value: 0x507, Tag: "fault-iptables-drop-30s-window"},
	{Value: 0x508, Tag: "fault-listen-fd-close-during-accept"},
	{Value: 0x509, Tag: "fault-tc-corrupt-1pct"},
	{Value: 0x50a, Tag: "fault-tc-duplicate-1pct"},
	{Value: 0x50b, Tag: "fault-sigstop-cont-pulses-100ms-cadence"},
	{Value: 0x50c, Tag: "fault-fd-pressure-ramp-down"},
	{Value: 0x50d, Tag: "fault-iptables-flap-1hz"},
	{Value: 0x50e, Tag: "fault-multi-tc+sigstop-coincident"},
	{Value: 0x50f, Tag: "fault-iouring-msg-ring-storm"},

	// Middleware interactions (0x600-0x60b).
	{Value: 0x600, Tag: "ratelimit-hot-key-burst-1k-rps"},
	{Value: 0x601, Tag: "ratelimit-key-eviction-mid-window"},
	{Value: 0x602, Tag: "session-rotate-during-active-request"},
	{Value: 0x603, Tag: "session-expiry-mid-handler"},
	{Value: 0x604, Tag: "jwt-rotate-signing-key-mid-request"},
	{Value: 0x605, Tag: "jwt-clock-skew-30s-future-iat"},
	{Value: 0x606, Tag: "csrf-token-rotation-burst"},
	{Value: 0x607, Tag: "cors-preflight-during-rate-limit"},
	{Value: 0x608, Tag: "etag-conditional-get-cache-miss-storm"},
	{Value: 0x609, Tag: "compress-payload-incompressible"},
	{Value: 0x60a, Tag: "compress-payload-highly-compressible-1m"},
	{Value: 0x60b, Tag: "timeout-handler-just-under-budget"},
}
