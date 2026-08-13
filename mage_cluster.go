//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Cluster orchestration targets. Mirrors celeris's mage_cluster.go but
// trims the "Cluster" prefix — every probatorium target is
// cluster-distributed by definition.
//
// Pristine semantics: every apt package installed by the deploy
// playbook is recorded in /tmp/celeris-bench-manifest.json; cleanup.yml
// uninstalls in reverse order. All transient state lives under /tmp on
// each node so a reboot wipes it. See ansible/README.md.

// Status prints quick health for each cluster node: ansible
// reachability, manifest state, and a short summary of any installed
// apt packages or staged binaries. Read-only — never mutates a node.
//
// Composed of two passes:
//
//  1. `ansible all -m shell -a uptime` — reachability check via the
//     same SSH path orchestration uses (Tailscale by default, LAN
//     when CLUSTER_USE_LAN=1).
//  2. manifestRead per host — pulls /tmp/celeris-bench-manifest.json
//     and prints apt_packages / binaries counts. A missing manifest
//     prints "no manifest" (a freshly provisioned host).
func Status() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	args := []string{
		"-i", "inventory.yml", "all",
		"-m", "shell",
		"-a", "uptime -p",
	}
	// CLUSTER_USE_LAN=1 routes via the 20G LACP fabric instead of
	// Tailscale. inventory.yml's `ansible_host` template gates on
	// `use_lan`, so we just need to pass that flag through.
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}
	cmd := exec.Command("ansible", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible reachability: %w", err)
	}

	hosts := []string{"msa2-server", "msa2-client", "msr1"}
	fmt.Printf("\n=== Manifest state ===\n")
	for _, h := range hosts {
		present, m, err := manifestRead(h)
		if err != nil {
			fmt.Printf("  %-12s  unreachable: %v\n", h, err)
			continue
		}
		if !present {
			fmt.Printf("  %-12s  no manifest (pristine)\n", h)
			continue
		}
		if m.IsEmpty() {
			fmt.Printf("  %-12s  manifest present, no installs (Go-only deploy)\n", h)
			continue
		}
		fmt.Printf("  %-12s  apt=%d toolchains=%d sysctls=%d\n",
			h, len(m.InstalledPackages), len(m.InstalledToolchains), len(m.PriorSysctl))
	}
	return nil
}

// Deploy cross-compiles every Go-side artifact and stages them on the
// cluster via deploy.yml. Steps:
//
//  1. Cross-compile probatorium's own four binaries (runner, loadgen,
//     observer, validator) for {linux/amd64, linux/arm64}.
//  2. For each Go competitor under servers/<name>/, cross-compile the
//     same two arches. DEPLOY_COMPETITORS controls the set:
//     "all" (default), "go-only" (skip non-Go), "none" (skip every
//     competitor — useful for validate-only matrix-tier jobs that
//     don't run bench), or a comma-separated list of competitor names.
//  3. ansible-playbook deploy.yml, passing every staged binary path
//     as an extra-var. The playbook copies binaries into
//     {{ bench_root }} on each host and writes the manifest.
//
// Env knobs:
//
//	CLUSTER_USE_LAN=1          — connect via the LAN IP map (LACP
//	                             fabric) instead of Tailscale overlay.
//	DEPLOY_COMPETITORS=all|... — competitor selection (see above).
//
// Idempotent: re-running with the same competitor set is a no-op for
// the manifest (deploy.yml re-asserts each file).
func Deploy() error {
	if err := requireAnsible(); err != nil {
		return err
	}

	stagingDir, err := os.MkdirTemp("", "probatorium-deploy-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	type bin struct {
		label  string
		module string
		pkg    string
		out    string
		arch   string
	}

	var jobs []bin
	// Core binaries land at {{ bench_root }}/<name> on the cluster.
	// The deploy playbook keys off the per-binary, per-arch extra-vars
	// listed below (e.g. runner_binary_amd64), so the set here MUST
	// stay in lockstep with `ansible/deploy.yml`'s push-* tasks.
	//
	// loadgen is amd64-only by default because msa2-client (the sole
	// loadgen host today) is amd64. The arm64 staging + per-arch
	// federation seam is gated behind arm64LoadgenEnabled() — see the
	// TODO(#168) block on that helper below.
	loadgenArchs := []string{"amd64"}
	if arm64LoadgenEnabled() {
		loadgenArchs = []string{"amd64", "arm64"}
	}
	coreBins := []struct {
		name  string
		archs []string
	}{
		{"runner", []string{"amd64", "arm64"}},
		{"loadgen", loadgenArchs},
		{"observer", []string{"amd64", "arm64"}},
		{"validator", []string{"amd64", "arm64"}},
		{"conformance", []string{"amd64", "arm64"}},
		{"validator-checker", []string{"amd64", "arm64"}},
		{"validator-replay", []string{"amd64", "arm64"}},
	}
	for _, b := range coreBins {
		for _, arch := range b.archs {
			jobs = append(jobs, bin{
				label:  b.name + " linux/" + arch,
				module: ".",
				pkg:    "./cmd/" + b.name,
				out:    filepath.Join(stagingDir, b.name+"-"+arch),
				arch:   arch,
			})
		}
	}

	// Validation refapps. Lives in its own go.mod (separate from the
	// root module because the refapp depends on a specific celeris
	// version via its own go.mod's `require` line) — we cross-compile
	// the binary so validate.yml can run it on bench_target alongside
	// the validator orchestrator.
	//
	// Each refapp produces a `refapp-<slug>-<arch>` binary the deploy
	// playbook pushes to {{ bench_root }}/refapps/<slug>.
	refappModules := []struct {
		slug   string
		module string
		archs  []string
	}{
		{
			slug:   "auth_session_ratelimit",
			module: "validation/refapp/auth_session_ratelimit",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// kitchen_sink covers 16+ stateless middlewares (recovery,
			// requestid, secure, cors, bodylimit, methodoverride,
			// rewrite, redirect, healthcheck, ratelimit, timeout,
			// circuitbreaker, idempotency, singleflight, basicauth +
			// per-route etag, cache). Added per probatorium#103.
			slug:   "kitchen_sink",
			module: "validation/refapp/kitchen_sink",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// auth_jwt_csrf covers the alternative-auth surface
			// (jwt, csrf, keyauth) that conflicts with kitchen_sink's
			// basicauth path. Added per probatorium#103.
			slug:   "auth_jwt_csrf",
			module: "validation/refapp/auth_jwt_csrf",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// driver_postgres: native postgres driver + session
			// + ratelimit on top. Tier 1 covers I-DRV-1 read-after-
			// write and pool-cap invariants. Added per #110.
			slug:   "driver_postgres",
			module: "validation/refapp/driver_postgres",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// driver_redis: same shape as driver_postgres but for
			// redis. Exercises CAS-free token-bucket via EVALSHA.
			// Added per #110.
			slug:   "driver_redis",
			module: "validation/refapp/driver_redis",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// driver_memcached: same shape for memcached. Token-
			// bucket uses CAS-loop retries since memcached has no
			// scripting. Added per #110.
			slug:   "driver_memcached",
			module: "validation/refapp/driver_memcached",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// observability: logger + metrics + otel mounted
			// together. Exposes /metrics in Prometheus text-plain
			// format + obs_log_drops gauge for the Tier 1 walker
			// to scrape. Added per #111.
			slug:   "observability",
			module: "validation/refapp/observability",
			archs:  []string{"amd64", "arm64"},
		},
		{
			// static_swagger_proxy: static (embed.FS) + swagger
			// (OpenAPI spec + UI) + proxy (X-Forwarded-For trust).
			// Covers the last untested middleware band.
			// Added per #112.
			slug:   "static_swagger_proxy",
			module: "validation/refapp/static_swagger_proxy",
			archs:  []string{"amd64", "arm64"},
		},
	}
	for _, r := range refappModules {
		for _, arch := range r.archs {
			jobs = append(jobs, bin{
				label:  "refapp " + r.slug + " linux/" + arch,
				module: r.module,
				pkg:    ".",
				out:    filepath.Join(stagingDir, "refapp-"+r.slug+"-"+arch),
				arch:   arch,
			})
		}
	}

	// Go competitors live under servers/<name>/ as their own modules
	// (matches mage_helpers.go celerisVersion logic — competitor
	// modules track their own deps). Filter via DEPLOY_COMPETITORS.
	competitorsArg := envOrDefault("DEPLOY_COMPETITORS", "all")
	goComps, err := selectGoCompetitors(competitorsArg)
	if err != nil {
		return err
	}
	for _, c := range goComps {
		for _, arch := range []string{"amd64", "arm64"} {
			jobs = append(jobs, bin{
				label:  "competitor " + c + " linux/" + arch,
				module: filepath.Join("servers", c),
				pkg:    ".",
				out:    filepath.Join(stagingDir, "competitor-"+c+"-"+arch),
				arch:   arch,
			})
		}
	}

	// Native competitors (rust / bun / python) compile on the bench
	// host because we cannot cross-compile a Rust crate that links
	// system C deps without a sysroot setup we don't have, and bun /
	// python aren't AOT-compiled at all. The dev Mac's job is to
	// tarball their source trees + describe how the cluster should
	// build each one.
	nativeComps, err := selectNativeCompetitors(competitorsArg)
	if err != nil {
		return err
	}
	// Union check: any CSV name that resolved to neither a Go nor a
	// native competitor is a typo / stale label and should fail the
	// deploy loudly rather than silently doing nothing.
	if competitorsArg != "" && competitorsArg != "all" && competitorsArg != "go-only" && competitorsArg != "none" {
		seen := make(map[string]bool, len(goComps)+len(nativeComps))
		for _, n := range goComps {
			seen[n] = true
		}
		for _, n := range nativeComps {
			seen[n] = true
		}
		for _, raw := range strings.Split(competitorsArg, ",") {
			n := strings.TrimSpace(raw)
			if n != "" && !seen[n] {
				return fmt.Errorf("DEPLOY_COMPETITORS: %q resolves to neither a Go competitor nor a native one", n)
			}
		}
	}
	competitorSources := make(map[string]map[string]any, len(nativeComps))
	for _, c := range nativeComps {
		spec, ok := nativeBuildSpecs[c]
		if !ok {
			return fmt.Errorf("native competitor %q has no build spec in nativeBuildSpecs", c)
		}
		tarball := filepath.Join(stagingDir, "native-"+c+".tar.gz")
		if err := tarballNativeSource(c, tarball); err != nil {
			return fmt.Errorf("tarball %s: %w", c, err)
		}
		entry := map[string]any{
			"lang":    spec.lang,
			"tarball": tarball,
		}
		if spec.buildCmd != "" {
			entry["build_cmd"] = spec.buildCmd
		}
		if spec.binaryRel != "" {
			entry["binary_rel"] = spec.binaryRel
		}
		if spec.moduleTarget != "" {
			entry["module_target"] = spec.moduleTarget
		}
		competitorSources[c] = entry
		fmt.Printf("Tarballed native source for %s (%s)...\n", c, spec.lang)
	}

	for _, j := range jobs {
		fmt.Printf("Cross-compiling %s...\n", j.label)
		if err := crossCompileGoBinary(j.module, j.pkg, j.out, j.arch); err != nil {
			return fmt.Errorf("cross-compile %s: %w", j.label, err)
		}
	}

	// The deploy playbook reads two surfaces:
	//
	//   1. Flat per-binary/per-arch vars for core binaries, e.g.
	//        runner_binary_amd64=<host path>
	//        runner_binary_arm64=<host path>
	//      Hyphens in binary names map to underscores (validator-checker
	//      -> validator_checker_binary_amd64) per Ansible/Jinja2
	//      identifier rules.
	//
	//   2. A `competitor_binaries` dict for Go competitors:
	//        {"gin": {"amd64": "...", "arm64": "..."}, ...}
	//      plus `competitor_set` echoed through so the playbook can
	//      filter (go-only | all | csv).
	//
	// We pass everything through a single JSON vars-file (`@vars.json`)
	// rather than many `key=value` --extra-vars. Ansible's `key=value`
	// form is string-only — passing a dict literal like
	// `competitor_binaries={"gin":...}` is silently coerced to a string
	// and `dict | list` on that string returns its characters, which is
	// exactly the failure mode we hit before this rewrite.
	coreSet := make(map[string]bool, len(coreBins))
	for _, b := range coreBins {
		coreSet[b.name] = true
	}
	vars := map[string]any{
		"competitor_set": competitorsArg,
	}
	competitorBinaries := make(map[string]map[string]string)
	refappBinaries := make(map[string]map[string]string)
	for _, j := range jobs {
		base := filepath.Base(j.out)
		logical := strings.TrimSuffix(base, "-"+j.arch)
		if coreSet[logical] {
			// runner / loadgen / observer / validator / conformance /
			// validator-checker / validator-replay
			varName := strings.ReplaceAll(logical, "-", "_") + "_binary_" + j.arch
			vars[varName] = j.out
			continue
		}
		if strings.HasPrefix(logical, "refapp-") {
			slug := strings.TrimPrefix(logical, "refapp-")
			if refappBinaries[slug] == nil {
				refappBinaries[slug] = make(map[string]string)
			}
			refappBinaries[slug][j.arch] = j.out
			continue
		}
		// Competitor: strip the "competitor-" prefix to recover the slug.
		slug := strings.TrimPrefix(logical, "competitor-")
		if competitorBinaries[slug] == nil {
			competitorBinaries[slug] = make(map[string]string)
		}
		competitorBinaries[slug][j.arch] = j.out
	}
	if len(competitorBinaries) > 0 {
		vars["competitor_binaries"] = competitorBinaries
	}
	if len(refappBinaries) > 0 {
		vars["refapp_binaries"] = refappBinaries
	}
	if len(competitorSources) > 0 {
		vars["competitor_sources"] = competitorSources
	}
	// gate_needs_docker is what deploy.yml's dbservices role guard
	// keys on. Driver-* scenarios (driver-pg-read / driver-redis-get /
	// driver-mc-get / driver-session-rw) need postgres + redis +
	// memcached containers; the bench/validate playbook flips
	// DEPLOY_NEEDS_DBSERVICES=1 on those runs to install + pull.
	//
	// Toolchain roles (rust / bun / python) install themselves under
	// bench_root unconditionally when their `competitors_with_lang_*`
	// set is non-empty — those have nothing to do with docker; they
	// gate on `competitor_sources` instead.
	if os.Getenv("DEPLOY_NEEDS_DBSERVICES") == "1" {
		vars["gate_needs_docker"] = true
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		// inventory.yml's ansible_host template picks up `use_lan` and
		// routes every host through its lan_ip. No per-host overrides
		// needed here.
		vars["use_lan"] = true
	}

	varsJSON, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	varsFile := filepath.Join(stagingDir, "deploy-vars.json")
	if err := os.WriteFile(varsFile, varsJSON, 0o600); err != nil {
		return err
	}

	args := []string{
		"-i", "inventory.yml",
		deployPlaybook,
		"--extra-vars", "@" + varsFile,
	}

	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	fmt.Printf("\n=== Deploy complete (%d binaries cross-compiled, %d Go competitors, %d native competitors) ===\n",
		len(jobs), len(competitorBinaries), len(competitorSources))
	return nil
}

// Cleanup tears down every cluster artifact: uninstalls apt packages
// recorded in the manifest, removes /tmp/celeris-bench/ and
// /tmp/celeris-results/, and deletes the manifest itself. Use after
// any failed/interrupted bench or validate run.
//
// Env knobs:
//
//	CLEANUP_HOSTS=all|<csv>    — limit cleanup to a subset (default:
//	                             all cluster hosts).
//
// Safe to run on a freshly provisioned host (no manifest → no-op).
func Cleanup() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	hosts := envOrDefault("CLEANUP_HOSTS", "all")
	args := []string{
		"-i", "inventory.yml",
		cleanupPlaybook,
		"--limit", hosts,
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// selectGoCompetitors returns the list of Go-based competitor
// directories under servers/ that match the DEPLOY_COMPETITORS spec.
// Recognised values:
//
//	"all"       — every directory under servers/ that has a go.mod
//	"go-only"   — same as "all" for Go competitors (the spec exists
//	              for symmetry with Rust/Bun/Python competitors that
//	              the ansible roles handle separately).
//	"<csv>"     — comma-separated list of names; each must exist
//	              under servers/ with a go.mod.
//
// Empty input behaves like "all" (matches the Deploy default). A CSV
// that names a non-Go competitor (axum, hono, fastapi, ...) silently
// drops it from THIS list because the native-competitor selector
// (selectNativeCompetitors) handles those entries on the parallel
// path. Names that exist in neither selector are caught by the union
// check in Deploy.
func selectGoCompetitors(spec string) ([]string, error) {
	all, err := scanGoCompetitors()
	if err != nil {
		return nil, err
	}
	switch spec {
	case "", "all", "go-only":
		return all, nil
	case "none":
		return nil, nil
	}
	have := make(map[string]bool, len(all))
	for _, n := range all {
		have[n] = true
	}
	var out []string
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// scanGoCompetitors lists every immediate child of servers/ that has
// a go.mod (i.e. is a Go competitor module). Non-Go competitors
// (rust/, bun/, python/) live under their own ansible roles and are
// staged from source on the cluster nodes themselves.
func scanGoCompetitors() ([]string, error) {
	entries, err := os.ReadDir("servers")
	if err != nil {
		// servers/ might not exist in a freshly cloned repo — treat
		// as zero competitors rather than an error so Deploy still
		// stages probatorium's own binaries.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// "common" is the shared scaffolding directory — not a
		// competitor itself.
		if e.Name() == "common" {
			continue
		}
		if _, err := os.Stat(filepath.Join("servers", e.Name(), "go.mod")); err == nil {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// nativeBuildSpec captures the deploy-time recipe for one non-Go
// adapter. The fields mirror what `ansible/tasks/build_native_
// competitor.yml` reads off `competitor_sources[<slug>]`:
//
//   - lang         — picks the dispatch path (rust → shell + symlink,
//     bun / python → role's build_competitor.yml).
//   - buildCmd     — generic shell run for rust; empty for bun /
//     python (their roles own the build).
//   - binaryRel    — path inside the source tree to the produced
//     artefact, used by the rust symlink step. Empty
//     for bun (role hardcodes `<src>/server`) and
//     python (no symlink, launcher lives at the
//     destination directly).
//   - moduleTarget — uvicorn import target for python adapters.
//     Defaulted to "app.server:app" in the role if
//     unset, but we set it explicitly for clarity.
type nativeBuildSpec struct {
	lang         string
	buildCmd     string
	binaryRel    string
	moduleTarget string
}

// nativeBuildSpecs is the deploy-time recipe table for every non-Go
// adapter under servers/. Keep in lockstep with servers/servers.go's
// Registry — a new NativeBinary adapter without an entry here will be
// rejected by Deploy with a clear error.
//
// binary_rel for rust adapters matches the per-adapter Cargo.toml
// `[[bin]] name` (cargo writes the binary as `target/<profile>/<name>`).
// The bench cluster always uses the release-fat profile defined in
// each Cargo.toml; -Ctarget-cpu=native goes via the ansible env block.
var nativeBuildSpecs = map[string]nativeBuildSpec{
	"axum": {
		lang:      "rust",
		buildCmd:  "cargo build --locked --profile release-fat",
		binaryRel: "target/release-fat/probatorium-axum-server",
	},
	"ntex": {
		lang:      "rust",
		buildCmd:  "cargo build --locked --profile release-fat",
		binaryRel: "target/release-fat/probatorium-ntex-server",
	},
	// hyper — same rust toolchain + release-fat profile as the framework
	// crates above; binary name from servers/hyper/Cargo.toml's [[bin]].
	"hyper": {
		lang:      "rust",
		buildCmd:  "cargo build --locked --profile release-fat",
		binaryRel: "target/release-fat/probatorium-hyper-server",
	},
	// drogon — C++ built on the bench host via the cpp role + libdrogon.
	// CMAKE_PREFIX_PATH / Drogon_DIR (env in build_native_competitor.yml)
	// point find_package(Drogon) at the bench-built prefix, so the
	// adapter's CMakeLists.txt needs no hard-coded prefix here.
	"drogon": {
		lang:      "cpp",
		buildCmd:  "cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j",
		binaryRel: "build/drogon-adapter",
	},
	// aspnet — .NET SDK (dotnet role). Framework-dependent publish; the
	// apphost binary is named via the csproj AssemblyName (aspnet).
	"aspnet": {
		lang:      "dotnet",
		buildCmd:  "dotnet publish aspnet.csproj -c Release -o publish",
		binaryRel: "publish/aspnet",
	},
	// zig_zap — Zig toolchain (zig role). ReleaseFast build emits the
	// binary under zig-out/bin/<exe name from build.zig>.
	"zig_zap": {
		lang:      "zig",
		buildCmd:  "zig build -Doptimize=ReleaseFast",
		binaryRel: "zig-out/bin/zig_zap",
	},
	"hono":    {lang: "bun"},
	"elysia":  {lang: "bun"},
	"fastapi": {lang: "python", moduleTarget: "app.server:app"},

	// ---- wave-6 native competitors ----
	"actix": {
		lang:      "rust",
		buildCmd:  "cargo build --locked --profile release-fat",
		binaryRel: "target/release-fat/probatorium-actix-server",
	},
	"starlette": {lang: "python", moduleTarget: "app.server:app"},
	"bunraw":    {lang: "bun"},
	"httpzig": {
		lang: "zig",
		// ReleaseSafe (not ReleaseFast): http.zig's NonBlocking worker hits an
		// `unreachable` under connection churn (churn-close) that becomes a
		// silent process-killing UB in ReleaseFast; ReleaseSafe makes it a
		// recoverable panic instead. This is the actual build the cluster uses.
		buildCmd:  "zig build -Doptimize=ReleaseSafe",
		binaryRel: "zig-out/bin/httpzig",
	},
	"lithium": {
		lang:      "cpp",
		buildCmd:  "cmake -S . -B build -DCMAKE_BUILD_TYPE=Release && cmake --build build -j",
		binaryRel: "build/lithium-adapter",
	},
	// h2o — c role builds libh2o into {bench}/h2o/prefix; the adapter Makefile
	// reads $H2O_PREFIX (env in build_native_competitor.yml) for -I/-L.
	"h2o": {
		lang:      "c",
		buildCmd:  "make H2O_PREFIX=\"$H2O_PREFIX\" CFLAGS_EXTRA=-march=native",
		binaryRel: "h2o-adapter",
	},
	// node + java are launcher langs (like bun): the role's build_competitor.yml
	// owns npm install / mvn package + launcher rendering, so the spec carries
	// only lang (buildCmd/binaryRel empty).
	"uws":     {lang: "node"},
	"fastify": {lang: "node"},
	"express": {lang: "node"},
	"vertx":   {lang: "java"},
	"netty":   {lang: "java"},
}

// selectNativeCompetitors returns the list of non-Go competitor slugs
// matching DEPLOY_COMPETITORS, mirroring selectGoCompetitors for
// nativeBuildSpecs. Recognised values:
//
//	"all"       — every entry in nativeBuildSpecs.
//	"go-only"   — empty list (no native competitors).
//	"<csv>"     — only the names that appear in nativeBuildSpecs.
//	              Names that DON'T appear are silently dropped here
//	              because they're handled by selectGoCompetitors.
//	              That keeps DEPLOY_COMPETITORS=gin,axum doing the
//	              expected thing (gin via Go path, axum via native).
//
// Empty input behaves like "all" (matches Deploy default).
func selectNativeCompetitors(spec string) ([]string, error) {
	if spec == "go-only" || spec == "none" {
		return nil, nil
	}
	all := make([]string, 0, len(nativeBuildSpecs))
	for name := range nativeBuildSpecs {
		all = append(all, name)
	}
	sort.Strings(all)
	if spec == "" || spec == "all" {
		return all, nil
	}
	have := make(map[string]bool, len(all))
	for _, n := range all {
		have[n] = true
	}
	var out []string
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// tarballNativeSource gzip-tars the *contents* of servers/<slug>/
// into dest. Important: the tarball has NO top-level directory entry
// — `ansible.builtin.unarchive` extracts into the configured dest
// without stripping a leading dir, so packaging `servers/axum/...`
// would land Cargo.toml at `competitors-src/axum/axum/Cargo.toml`.
// Using `-C servers/<slug> .` instead drops Cargo.toml at the right
// level (`competitors-src/axum/Cargo.toml`).
//
// Excludes caches we'd never want to ship:
//
//   - node_modules/  (bun resolves these fresh on the cluster)
//   - dist/          (bun build output; built per-host)
//   - target/        (cargo build cache; built per-host)
//   - .venv/         (uv venv; built per-host)
//   - .git/, *.pyc, __pycache__/ (general noise)
//
// Uses `tar` directly because the stdlib archive/tar doesn't speak
// gzip and the macOS BSD tar handles the exclude patterns we need.
func tarballNativeSource(slug, dest string) error {
	src := filepath.Join("servers", slug)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("native source dir %s: %w", src, err)
	}
	// The destination tarball must not exist yet — tar will overwrite
	// it but we want a clean state.
	_ = os.Remove(dest)

	// Purge any on-disk macOS AppleDouble files under the source tree first.
	// COPYFILE_DISABLE (below) only stops tar SYNTHESIZING `._*` from xattrs;
	// `._*` files that already exist on disk (left by a Finder/cp operation)
	// would still be archived and then break the on-Linux build (the C#
	// compiler globs `._Program.cs` as a binary .cs source → CS2015).
	prune := exec.Command("find", src, "-name", "._*", "-delete")
	prune.Stderr = os.Stderr
	_ = prune.Run()

	cmd := exec.Command("tar", "czf", dest,
		"--exclude=node_modules",
		"--exclude=dist",
		"--exclude=target",
		"--exclude=.venv",
		"--exclude=.git",
		"--exclude=__pycache__",
		"--exclude=*.pyc",
		// Compiled-language build outputs: never ship stale local artefacts
		// (a leaked dotnet publish/ tree triggers MSB1011 "more than one
		// project file"; stale zig-out/build dirs confuse fresh builds).
		"--exclude=bin",
		"--exclude=obj",
		"--exclude=publish",
		"--exclude=build",
		"--exclude=zig-out",
		"--exclude=.zig-cache",
		// macOS AppleDouble resource-fork files (`._Program.cs`, …). BSD tar
		// embeds them by default; on Linux they extract as real files and the
		// C# compiler globs `._*.cs` as binary source → CS2015. Belt: the
		// --exclude pattern; suspenders: COPYFILE_DISABLE below stops tar
		// emitting them at all.
		"--exclude=._*",
		"-C", src,
		".",
	)
	cmd.Stderr = os.Stderr
	// COPYFILE_DISABLE=1 tells macOS BSD tar not to write AppleDouble (`._*`)
	// entries — the canonical fix for "binary file instead of a text file"
	// breakage when a mac-built tarball is extracted + compiled on Linux.
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	return cmd.Run()
}

// runHostsParallel fans out fn over hosts and returns the first
// non-nil error per host as a single combined error. Used by targets
// that orchestrate multiple hosts independently (each host gets its
// own ansible-playbook subprocess).
func runHostsParallel(hosts []string, fn func(host string) error) error {
	type res struct {
		host string
		err  error
	}
	results := make(chan res, len(hosts))
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			results <- res{host: host, err: fn(host)}
		}(h)
	}
	wg.Wait()
	close(results)
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.host, r.err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("parallel run failed on %d host(s): %s",
			len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// ── arm64 loadgen federation seam (#168) ────────────────────────────
//
// Today the bench fabric drives BOTH arch targets from a single amd64
// loadgen host (msa2-client): the amd64 server (msa2-server) is driven
// natively, but the arm64 server (msr1) is also driven by that amd64
// client over the 20G fabric, and the two arch passes run SERIALLY
// (BENCH_TARGET=both = two bench.yml passes back-to-back). That
// serialization is the ~2× wall-clock the 24h budget (#166) fights.
//
// The fix is to stand up a SECOND, arm64-native loadgen instance so
// each arch is driven by a same-arch client, and to run the two passes
// CONCURRENTLY (one playbook run per arch, fanned out via
// runHostsParallel). Each instance produces its own per-cell
// loadgen.Result with a V2-compressed HDR histogram; mergeBenchResults
// (mage_bench.go) already base64-encodes each run's histogram and
// report.Aggregate / report.mergeHistograms (report/aggregate.go:258)
// merges them ACROSS RUNS within an arch. Arches stay SEPARATE cells in
// the docs tree (arch is a tree dimension), so no cross-arch merge is
// needed — the federation's only job is to produce arm64 histograms
// with an arm64 driver instead of amd64-driven-arm64 data.
//
// TODO(#168): this seam is INERT by default and BLOCKED on the
// loadgen-repo (goceleris/loadgen) building a linux/arm64 binary.
// crossCompileGoBinary already cross-compiles ./cmd/loadgen for any
// GOARCH, but loadgen's own deps must build clean under GOARCH=arm64
// first. The four seam steps, all gated behind arm64LoadgenEnabled():
//
//   1. Stage an arm64 loadgen binary (the loadgenArchs branch in
//      Deploy above adds "arm64" when the flag is on; deploy.yml must
//      grow a loadgen_binary_arm64 push to msr1 to match).
//   2. Run two independent loadgen instances, one per arch, each
//      driving a SAME-arch target (recommended over a 2-node
//      single-peer federation: no peer-coordination protocol, and the
//      merge primitive already exists). The v1 topology co-locates the
//      arm64 driver on msr1 itself (documented measurement caveat:
//      driver+SUT contention); the clean topology is a dedicated arm64
//      driver box.
//   3. Drive the two arch passes CONCURRENTLY via loadgenFederation
//      below (mirrors the VALIDATE_PARALLEL fan-out precedent).
//   4. Reuse report.mergeHistograms UNCHANGED for across-run merge.
//
// Flipping budget.Profile.ArchParallel=true (the #168 wall-clock win)
// is GATED on this seam being live — until loadgen ships linux/arm64,
// the budget calculator must run with ArchParallel=false (serial).

// arm64LoadgenEnabled reports whether the arm64-native loadgen
// federation seam is active. It is OFF by default so deploy/bench keep
// today's amd64-driven behaviour byte-for-byte; the loadgen-repo arm64
// build (TODO #168 above) flips this on via PROBATORIUM_ARM64_LOADGEN=1
// once goceleris/loadgen ships a working linux/arm64 binary.
func arm64LoadgenEnabled() bool {
	return os.Getenv("PROBATORIUM_ARM64_LOADGEN") == "1"
}

// archLoadgenHost maps a bench target host to the loadgen host that
// drives it. With the federation seam OFF, every target is driven by
// the single amd64 loadgen host (today's behaviour). With it ON, the
// arm64 target (msr1) is driven by a same-arch instance — co-located on
// msr1 in the v1 seam (see TODO #168 measurement caveat).
func archLoadgenHost(target string) string {
	const amd64Loadgen = "msa2-client"
	if !arm64LoadgenEnabled() {
		return amd64Loadgen
	}
	switch target {
	case "msr1":
		// TODO(#168): co-located driver+SUT on msr1 pollutes the
		// measurement; swap for a dedicated arm64 driver box when one
		// joins the fabric.
		return "msr1"
	default:
		return amd64Loadgen
	}
}

// loadgenFederation runs the per-arch bench passes for BENCH_TARGET=both
// either SERIALLY (today, federation seam off) or CONCURRENTLY (one
// loadgen instance per arch, federation seam on — the #168 win). It is
// the single fan-out point so mage_bench.go's "both" branch can call it
// without knowing whether the seam is live.
//
// runPass drives one arch target end-to-end (deploy-already-done,
// playbook + fetch + per-arch merge). The two passes write to SEPARATE
// per-arch results dirs, so concurrent execution never races on output
// and the existing per-arch Publish path ships one tree per arch.
//
// NOTE: this helper is the structural seam; mage_bench.go's "both"
// branch keeps its current serial loop until the loadgen-repo arm64
// dependency lands and arm64LoadgenEnabled() returns true. Keeping it
// here (rather than inlined) means the parallel path is reviewable and
// testable independently of flipping the flag.
func loadgenFederation(targets []string, runPass func(target string) error) error {
	if !arm64LoadgenEnabled() {
		// Serial: preserve today's exact wall-clock + ordering so the
		// seam is fully inert until the arm64 loadgen build lands.
		for _, t := range targets {
			if err := runPass(t); err != nil {
				return err
			}
		}
		return nil
	}
	// Concurrent: each arch driven by its own same-arch loadgen
	// instance (archLoadgenHost) — mirrors runHostsParallel's fan-out.
	return runHostsParallel(targets, runPass)
}
