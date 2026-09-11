module github.com/goceleris/probatorium/servers/celeris

go 1.27.0

require (
	github.com/goceleris/celeris v1.5.12-0.20260911041144-be12a9837711
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
