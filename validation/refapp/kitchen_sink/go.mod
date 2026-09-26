module github.com/goceleris/probatorium/validation/refapp/kitchen_sink

go 1.27.0

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20260920100717-9f4d89b171db
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
