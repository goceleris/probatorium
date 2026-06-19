# probatorium libreactor adapter — build manifest.
#
# Produces ./libreactor-adapter, a minimal HTTP/1.1 epoll server built on
# libreactor 3.x (fredrikwidlund/libreactor) — the historic TechEmpower #1
# C framework. The bench cluster builds libreactor from source into a
# pristine prefix under {bench_root}/libreactor/prefix (see
# ansible/roles/c), then invokes this Makefile with PREFIX pointed at it:
#
#   make PREFIX={bench_root}/libreactor/prefix
#
# locally you can install libreactor to /usr/local (autogen + configure +
# make install) and just run `make`.
#
# Link line: libreactor 3.x ships a SINGLE static archive `libreactor`
# (there is NO separate libdynamic / libclo — those were pre-2.x packages).
# The server module embeds an SSL_CTX and pulls <openssl/ssl.h>, so the
# program links OpenSSL too: -lreactor -lssl -lcrypto. -pthread covers the
# library's async/resolver threads.

PREFIX  ?= /usr/local
CC      ?= cc

# -I/-L resolve against the libreactor install prefix. -O3 -flto
# -march=native mirror libreactor's own example/Makefile.am AM_CFLAGS so
# the adapter is benched at the framework's intended optimization level.
CFLAGS  ?= -O3 -flto -march=native -Wall -Wextra
CFLAGS  += -I$(PREFIX)/include
LDFLAGS ?= -flto
LDFLAGS += -L$(PREFIX)/lib
LDLIBS  := -lreactor -lssl -lcrypto -pthread

BIN     := libreactor-adapter
SRC     := main.c

$(BIN): $(SRC)
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ $(SRC) $(LDLIBS)

clean:
	rm -f $(BIN)

.PHONY: clean
