module github.com/goceleris/probatorium/validation/refapp/driver_memcached

go 1.27.2

require (
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

require (
	github.com/goceleris/celeris v1.5.12-0.20261013133423-7d9ff01dab67
	github.com/goceleris/probatorium/validation/refapp/internal/debugvars v0.0.0
)

replace github.com/goceleris/probatorium/validation/refapp/internal/debugvars => ../internal/debugvars
