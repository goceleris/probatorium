module github.com/goceleris/probatorium/servers/celeris

go 1.27.0

require (
	github.com/goceleris/celeris v1.5.12-0.20260915033145-30b637c82553
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
