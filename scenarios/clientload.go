package scenarios

import "sort"

// ClientLoad says how much of the shared loadgen host's capacity a scenario's
// cell takes while it runs. Both arch passes of a BENCH_TARGET=both run are
// driven from the one msa2-client (probatorium#442); a cell of class
// LoadHeavy must never run at the same time as a LoadHeavy cell of the other
// arch (the runner takes a host-wide lock around it, cmd/runner/clientlock.go).
// A LoadLight cell takes no lock and may overlap anything.
type ClientLoad string

const (
	// LoadHeavy cells drive the client at a rate where the other arch's
	// concurrent load measurably changes the result: link-bound rows (the
	// 2x10G bond carries both arches), request rates of several hundred
	// thousand per second (client CPU and latency channel, probatorium#442).
	LoadHeavy ClientLoad = "heavy"
	// LoadLight cells stay far below both channels on BOTH arches.
	LoadLight ClientLoad = "light"
)

// clientLoadTable is the class of every registered scenario. It is explicit
// on purpose: ClientLoadOf treats a name that is not in it as heavy, and
// TestEveryScenarioHasAClientLoadClass fails until a new scenario is added
// here, so a new row can never silently run unprotected.
//
// The demand figures are over the 45 / 23 / 16 / 13 columns that ran each row
// in the published v1.5.8 0829 run (benchmark-tier 33256654220), p90 over
// columns of saturation requests/s, x86 | arm64, and the larger direction of
// the body bytes in Gbit/s (scripts/demand_by_row.py in the P442 lane
// evidence). Rule: a row is light when its p90 stays at or below 150k
// requests/s and 4 Gbit/s on both arches. Everything else, and everything
// unmeasured, is heavy.
//
// The link channel (probatorium#442, own + other arch traffic = 19.1-20.1
// Gbit/s on the 20 Gbit/s bond) can only bind on the bandwidth rows. The
// client channel was measured on static rows of 128 and 256 connections
// (goceleris/probatorium#442 comment on run 36441790519; evidence/bench-
// 36441790519/rank/skeptic/out_s18_cpu_contention2.txt: slope -0.31 at 128c,
// -0.18 at 256c, partial r -0.92 to -0.97, over arm64 demand of 174-321k
// requests/s), where an x86 cell lost 0.18-0.31 requests/s per concurrent
// arm64 request. Nothing in this table was measured on the light rows.
//
// What the light class leaves unprotected, by that coefficient and the demand
// figures below (it is an extrapolation, not a measurement on these rows):
//   - an x86 heavy cell sharing the client with the arm64 light rows: two of
//     them reach 122-126k (ws-hub-broadcast-128 frames/s, sse-fanout-128
//     events/s), which is 22-39k requests/s lost from a 650k requests/s cell,
//     3.5-6%; the others stay at or below 30k, under 1.5%;
//   - an arm64 heavy cell (about 400k requests/s) sharing it with the x86
//     light rows, which reach 120-140k: the coefficient was never measured in
//     this direction; if it were symmetric, 25-43k, 6-11%;
//   - the light rows' OWN measurements under the other arch's heavy cells:
//     12 of the 15 rated rows are light (budget.RatedScenarios), and the
//     order under policy heavy puts 173 rated light cells per arch under a peer heavy
//     cell (cmd/runner TestFullGridArchPlan).
//
// docs/ARCH-EXCLUSION-VALIDATION.md is built to measure all three, and
// BENCH_ARCH_EXCLUSIVE=all (the default) removes them.
var clientLoadTable = map[string]ClientLoad{
	// bandwidth rows: 14-18 Gbit/s on x86, 7-10 on arm64.
	"post-4k":       LoadHeavy, // 548k | 226k rps, 18.0 | 7.4 Gbit/s
	"post-4k-h2":    LoadHeavy, // 428k | 233k rps, 14.0 | 7.6 Gbit/s
	"ws-large-echo": LoadHeavy, // 35k | 20k rps, 18.3 | 10.4 Gbit/s
	"get-json-1k":   LoadHeavy, // 952k | 361k rps, 7.8 | 3.0 Gbit/s

	// high request-rate rows (client CPU / latency channel).
	"get-json":              LoadHeavy, // 1.03M | 380k rps
	"get-json-h2":           LoadHeavy, // 541k | 398k rps
	"get-simple":            LoadHeavy, // 967k | 377k rps
	"get-simple-128c":       LoadHeavy, // 943k | 400k rps
	"get-simple-256c":       LoadHeavy, // 1.15M | 423k rps
	"get-simple-512c":       LoadHeavy, // 1.32M | 427k rps
	"get-simple-1024c":      LoadHeavy, // 1.39M | 422k rps
	"ws-echo":               LoadHeavy, // 796k | 224k rps
	"ws-hub-broadcast-1024": LoadHeavy, // 961k | 324k frames/s read
	"sse-fanout-1024":       LoadHeavy, // 997k | 872k events/s read

	// not scheduled on the cluster today (no TLS terminator, -tls-terminator
	// empty), so never measured: heavy until a run says otherwise.
	"tls-get-json":          LoadHeavy,
	"tls-concurrency-128c":  LoadHeavy,
	"tls-concurrency-1024c": LoadHeavy,

	// light rows: p90 <= 150k rps and <= 4 Gbit/s on both arches.
	"get-simple-1c":         LoadLight, // 22k | 9k rps (one connection)
	"churn-close":           LoadLight, // 140k | 24k rps
	"ws-hub-broadcast-128":  LoadLight, // 127k | 123k frames/s read, server tick bound
	"sse-fanout-128":        LoadLight, // 122k | 126k events/s read, server tick bound
	"driver-mc-get":         LoadLight, // 108k | 24k rps, 3.5 | 0.8 Gbit/s
	"driver-mc-multiget":    LoadLight, // 96k | 25k rps
	"driver-mc-set":         LoadLight, // 130k | 29k rps
	"driver-pg-read":        LoadLight, // 82k | 20k rps
	"driver-pg-read-range":  LoadLight, // 54k | 16k rps, 1.4 | 0.4 Gbit/s
	"driver-pg-update-tx":   LoadLight, // 10k | 2k rps
	"driver-pg-write":       LoadLight, // 92k | 22k rps
	"driver-redis-get":      LoadLight, // 74k | 22k rps, 2.4 | 0.7 Gbit/s
	"driver-redis-pipeline": LoadLight, // 54k | 17k rps
	"driver-redis-set":      LoadLight, // 103k | 23k rps
	"driver-session-rw":     LoadLight, // 53k | 13k rps
}

// ClientLoadOf returns the client-load class of the scenario called name.
// A name missing from the table is heavy: an unclassified row must take the
// lock, never skip it.
func ClientLoadOf(name string) ClientLoad {
	if c, ok := clientLoadTable[name]; ok {
		return c
	}
	return LoadHeavy
}

// ClassifiedScenarios returns every scenario name the table classifies,
// sorted. Tests use it to compare the table against the registry.
func ClassifiedScenarios() []string {
	out := make([]string, 0, len(clientLoadTable))
	for n := range clientLoadTable {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
