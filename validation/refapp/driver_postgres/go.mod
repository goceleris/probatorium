module github.com/goceleris/probatorium/validation/refapp/driver_postgres

go 1.27.0

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20260915174751-c40d0cb9d4b7
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
