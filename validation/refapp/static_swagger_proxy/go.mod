module github.com/goceleris/probatorium/validation/refapp/static_swagger_proxy

go 1.27.0

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20260908205326-ea3d528fcd4b
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
