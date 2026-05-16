package main

import "github.com/goceleris/celeris"

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
