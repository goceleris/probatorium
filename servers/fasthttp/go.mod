module github.com/goceleris/probatorium/servers/fasthttp

go 1.26.3

require (
	github.com/goceleris/probatorium v0.0.0-00010101000000-000000000000
	github.com/valyala/fasthttp v1.71.0
)

require (
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
)

replace github.com/goceleris/probatorium => ../..
