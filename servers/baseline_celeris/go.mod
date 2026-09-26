module github.com/goceleris/probatorium/servers/baseline_celeris

go 1.27.0

require (
	github.com/goceleris/celeris v1.5.8
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
