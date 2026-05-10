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
