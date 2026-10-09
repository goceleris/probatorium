module github.com/goceleris/probatorium/validation/refapp/static_swagger_proxy

go 1.27.0

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20261003135342-ed3b7fbcff33
	github.com/goceleris/probatorium/internal/exitguard v0.0.0
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/internal/exitguard => ../../../internal/exitguard
replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
