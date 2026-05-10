module github.com/goceleris/probatorium/servers/stdhttp

go 1.26.3

require (
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.54.0
)

require golang.org/x/text v0.37.0 // indirect

replace github.com/goceleris/probatorium => ../..
