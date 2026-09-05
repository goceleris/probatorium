# Contributing

## Setup

```sh
go mod download
go install github.com/magefile/mage@latest
```

Linux is required for any `mage Bench` / `mage Validate` / `mage Soak` since the cluster targets are Linux. macOS is fine for `go build` + `go test ./...`.

## Build & test

```sh
go build ./...
go vet ./...
go test ./...
mage -compile /tmp/check  # confirm magefile compiles
```

## Cluster ops

`mage Status` is read-only — safe to run anytime. Anything else (`Deploy` / `Bench` / `Validate` / `Soak` / `Fuzz` / `Cleanup`) drives ansible against the cluster. Inventory lives at `ansible/inventory.yml`.

If a run crashes mid-flight, `mage Cleanup` is the recovery path — it reads `/tmp/celeris-bench-manifest.json` on each host and undoes only what we installed.

## PRs

One wave per branch / PR. Branch naming: `feat/<short-slug>`, `fix/<short-slug>`. Each PR description should reference the wave issue it closes.

## How changes get merged

The same rule applies across the goceleris repositories:

- Every change lands through a pull request against `main`; nothing is pushed to `main` directly.
- The required checks must be green — `ci-ok` (the fan-in over the whole Test matrix) and Lint — and Dependabot alerts must not regress.
- A code owner (see `.github/CODEOWNERS`) reviews and approves. The maintainer merges; contributors do not self-merge.
- Commit messages use a conventional prefix (`feat:`, `fix:`, `ci:`, `docs:`, `chore:`, ...) and explain *why*, not just what.

### Cluster runs need the `cluster-ok` label

The PR Validation workflow (`.github/workflows/matrix-pr-tier.yml`) executes on the self-hosted bench cluster, so it does not run on every push. A maintainer reviews the diff and adds the `cluster-ok` label; only then does the cluster job start, and only for branches that live in this repository (fork PRs never reach the cluster). The label is consumed per event — re-add it after any push you want re-validated. Cluster runs queue behind any in-flight nightly, weekend-soak or benchmark run and are never cancelled.
