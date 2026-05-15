package main

import "github.com/goceleris/celeris"

// resolveEngine maps the -engine flag to a celeris.EngineType.
// auth_jwt_csrf doesn't mount websocket or sse, so the celeris#273
// hijack/streaming bug doesn't apply — `auto` picks iouring on Linux
// for production parity.
func resolveEngine(name string) celeris.EngineType {
	switch name {
	case "iouring":
		return celeris.IOUring
	case "epoll":
		return celeris.Epoll
	case "std":
		return celeris.Std
	case "adaptive":
		return celeris.Adaptive
	case "auto":
		if isLinux() {
			return celeris.IOUring
		}
		return celeris.Std
	}
	return celeris.Std
}
