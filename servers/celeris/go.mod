module github.com/goceleris/probatorium/servers/celeris

go 1.26.3

require (
	github.com/goceleris/celeris v1.4.11-0.20260524235534-f3f62bbf48e0
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
