// Versions of the Go tools the workflows run, kept out of the probatorium
// module so they add nothing to its dependency graph. Each is a `tool`
// directive, pinned by go.sum and bumped by Dependabot (.github/dependabot.yml).
// The workflows install mage from here with
// `go install -modfile=$GITHUB_WORKSPACE/.github/tools/go.mod github.com/magefile/mage`
// and run actionlint with
// `go tool -modfile=$GITHUB_WORKSPACE/.github/tools/go.mod actionlint`.
// This module is never imported or released (probatorium#458).
module probatorium-ci-tools

go 1.27.0

tool (
	github.com/magefile/mage
	github.com/rhysd/actionlint/cmd/actionlint
)

require (
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/magefile/mage v1.17.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.21 // indirect
	github.com/mattn/go-shellwords v1.0.12 // indirect
	github.com/rhysd/actionlint v1.7.12 // indirect
	github.com/robfig/cron/v3 v3.0.1 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.3 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
)
