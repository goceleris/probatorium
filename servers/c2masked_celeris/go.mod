module github.com/goceleris/probatorium/servers/c2masked_celeris

go 1.27.2

require (
	github.com/goceleris/celeris v1.5.12-0.20261016183332-037cba2d081d
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.61.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
