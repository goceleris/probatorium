#!/usr/bin/env bash
# Stage everything the run needs BEFORE the measured part, so no build or download happens while measuring.
# Run from the HttpArena clone (after host_adapt.py). Inbound only: registry pulls, git clone of MDA2AV/gcannon,
# the zrk release tarball, `go mod download` inside the framework builds. Nothing is pushed anywhere.
set -euo pipefail
GCANNON_SHA=${GCANNON_SHA:-11b8f116958913f0c6e74a6fd6a73b5a12204b35}   # MDA2AV/gcannon main on 2026-11-22; the Dockerfile default is the moving `main`
FWS=${FWS:-"celeris celeris-default celeris-epoll celeris-iouring celeris-tlschild celeris-ceiling gin fiber echo go-fasthttp go-stdlib fiber-websocket"}
docker pull golang:1.27.4-alpine; docker pull golang:1.26-alpine; docker pull golang:1.27.3-alpine
docker pull alpine:3.23; docker pull ubuntu:24.04; docker pull postgres:18
docker build -t gcannon:latest --build-arg GCANNON_REF="$GCANNON_SHA" -f docker/gcannon.Dockerfile docker
docker build -t h2load:1.69.0 -f docker/h2load.Dockerfile docker
docker build -t wrk:local     -f docker/wrk.Dockerfile docker
docker build -t zrk:local     -f docker/zrk.Dockerfile docker
# benchmark.sh (LOADGEN_DOCKER=true) builds any of its five loadgen images that is missing; h2load-h3 is a 5-10 minute
# source build and no h3 profile runs here, so a placeholder tag keeps it from being built. h3 profiles are NOT run.
docker image inspect h2load-h3:local >/dev/null 2>&1 || docker tag ubuntu:24.04 h2load-h3:local
# an entry with an executable build.sh (celeris-epoll / celeris-iouring) is built by it, as benchmark.sh does
for fw in $FWS; do if [ -x "frameworks/$fw/build.sh" ]; then "frameworks/$fw/build.sh"; else docker build -t "httparena-$fw" "frameworks/$fw"; fi; done
docker images --format '{{.Repository}}:{{.Tag}}\t{{.Size}}' | sort > preview-images.txt; cat preview-images.txt
