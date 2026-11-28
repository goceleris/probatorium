module idlecell

go 1.27.4

// celeris is replaced at build time (tools/idlecell/build.sh: go mod edit -replace) by the worktree of the variant
// being built, so one source builds every arm.
require github.com/goceleris/celeris v0.0.0
