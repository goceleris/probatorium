# probatorium h2o adapter (C, libh2o) build.
#
# Produces ./h2o-adapter, a single-file C server linked against libh2o's
# event-loop variant (libh2o-evloop) plus its transitive deps (OpenSSL,
# zlib, wslay). Mirrors the drogon adapter's "build natively on the bench
# host" model: the ansible "c" role compiles + installs libh2o into a
# bench-root prefix and exports H2O_PREFIX / PKG_CONFIG_PATH; this Makefile
# resolves the include + link flags from there.
#
# Resolution order for the h2o flags:
#   1. pkg-config libh2o-evloop  (the .pc the cmake install drops in
#      $PREFIX/lib/pkgconfig — preferred; carries -I/-L automatically).
#   2. H2O_PREFIX fallback (-I$PREFIX/include -L$PREFIX/lib -lh2o-evloop)
#      for a prefix that has no .pc on PKG_CONFIG_PATH.
#
# libh2o-evloop is a STATIC archive whose .pc Libs: line does NOT carry its
# transitive link deps (a static .a records no dependency metadata), so the
# final adapter link must name them explicitly. The evloop archive bundles
# picotls/quicly/picohttpparser and references symbols from:
#   -lssl -lcrypto   OpenSSL (h2o's FIND_PACKAGE(OpenSSL REQUIRED))
#   -lz              zlib    (FIND_PACKAGE(ZLIB REQUIRED))
#   -lbrotlienc -lbrotlidec  Brotli — REQUIRED: h2o only INSTALLS
#                    libh2o-evloop.a when system brotli is found
#                    (BROTLI_FOUND gates the INSTALL(TARGETS ...)), so a
#                    prefix that contains the archive was built WITH brotli
#                    and the archive references brotli symbols.
#   -lwslay          websocket framing (libh2o links it when libwslay is
#                    present; the archive references it). Harmless to keep on
#                    the line — our embedding never mounts the ws handler, but
#                    the static archive still pulls the symbols.
# plus -lm -lpthread -ldl. If a given prefix was built without one of these
# (e.g. no system wslay), drop it via H2O_EXTRA_LIBS on the make line.
#
# Override knobs (make VAR=...):
#   CC             compiler            (default cc)
#   H2O_PREFIX     libh2o install dir  (used only if pkg-config misses)
#   H2O_EXTRA_LIBS transitive deps
#                  (default -lssl -lcrypto -lz -lbrotlienc -lbrotlidec -lwslay)
#   CFLAGS_EXTRA   appended to CFLAGS  (e.g. -march=native from the role)

CC ?= cc
BIN := h2o-adapter
SRC := main.c

PKG_CONFIG ?= pkg-config
H2O_PREFIX ?= /usr/local
H2O_EXTRA_LIBS ?= -lssl -lcrypto -lz -lbrotlienc -lbrotlidec -lwslay

# h2o include/link flags from pkg-config when the .pc is visible, else the
# H2O_PREFIX fallback. `pkg-config --exists` is the probe.
H2O_PC := $(shell $(PKG_CONFIG) --exists libh2o-evloop 2>/dev/null && echo yes)
ifeq ($(H2O_PC),yes)
  H2O_CFLAGS := $(shell $(PKG_CONFIG) --cflags libh2o-evloop)
  H2O_LIBS := $(shell $(PKG_CONFIG) --libs libh2o-evloop)
else
  H2O_CFLAGS := -I$(H2O_PREFIX)/include
  H2O_LIBS := -L$(H2O_PREFIX)/lib -lh2o-evloop
endif

# H2O_USE_LIBUV=0 selects the evloop backend in the installed headers
# (libh2o-evloop). Without it h2o/socket.h falls back to the libuv binding
# and pulls in <uv.h>, which the evloop variant does not ship.
#
# -O3 matches the optimization level libreactor bakes into its own Makefile.
# The bench role passes CFLAGS_EXTRA=-march=native (build_native_competitor's
# c family has no implicit CFLAGS env), so the adapter is benched native.
#
# -std=gnu11 + -D_GNU_SOURCE: h2o's public headers use POSIX/GNU symbols
# (posix_memalign in h2o/memory.h, struct addrinfo in h2o/hostinfo.h) that
# strict ISO C (-std=c11 → __STRICT_ANSI__) hides — h2o's own build defines
# _GNU_SOURCE, so the embedding must too or those headers fail to compile.
CFLAGS ?= -O3 -std=gnu11
CFLAGS += -D_GNU_SOURCE -DH2O_USE_LIBUV=0 $(H2O_CFLAGS) $(CFLAGS_EXTRA)
LDLIBS := $(H2O_LIBS) $(H2O_EXTRA_LIBS) -lpthread -lm -ldl

.PHONY: all clean
all: $(BIN)

$(BIN): $(SRC)
	$(CC) $(CFLAGS) -o $@ $(SRC) $(LDLIBS)

clean:
	rm -f $(BIN)
