package main

import "github.com/goceleris/celeris"

// resolveEngine maps the -engine flag to a celeris.EngineType. Same
// shape as auth_session_ratelimit; default `auto` falls back to std
// on every platform per the celeris#273 workaround until iouring +
// epoll handle hijack / streaming responses correctly.
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
		// kitchen_sink doesn't mount websocket or sse middlewares,
		// so iouring + epoll DO work cleanly here even with
		// AsyncHandlers=true. Pick iouring on Linux for production
		// parity. (auth_session_ratelimit can't because of #273.)
		if isLinux() {
			return celeris.IOUring
		}
		return celeris.Std
	}
	return celeris.Std
}
