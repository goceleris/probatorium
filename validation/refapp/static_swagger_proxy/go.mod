module github.com/goceleris/probatorium/validation/refapp/static_swagger_proxy

go 1.27.3

require (
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20261021042539-713b40dfdd0d
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
