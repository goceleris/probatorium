module github.com/goceleris/probatorium/servers/celeris

go 1.26.4

require (
	github.com/goceleris/celeris v1.4.16-0.20260612132018-7512c15cd341
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
)

require (
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
