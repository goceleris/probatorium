## Summary

What changes and why.

## Changes

-

## Test Plan

The Test and Lint workflows run these; tick what you ran locally:

- [ ] `go test -race -count=1 ./...` in the root module, and in every changed module under `servers/` or `validation/refapp/`
- [ ] `go test -race -count=1 -tags mage ./...` if a magefile, or anything a magefile calls, changed
- [ ] `go vet ./...`, `golangci-lint run` and `gofmt -l .` are clean, and `mage -compile /tmp/mage-check` succeeds
- [ ] If a workflow changed: `actionlint`, and a gate's new or changed check is shown to pass on good input and fail on bad
- [ ] If the report schema changed: `SchemaVersion` is bumped and re-pinned in `report/schema_test.go`, `mage_bench_sutenv_test.go` and `validation/runner_test.go`
- [ ] Cluster tier needed? Say which one; tiers are dispatched by hand.

Closes #
