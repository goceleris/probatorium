cmake_minimum_required(VERSION 3.16)
project(lithium_adapter CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

if(NOT CMAKE_BUILD_TYPE)
    set(CMAKE_BUILD_TYPE Release)
endif()

# --- lithium single-header dependency -------------------------------------
#
# matt-42/lithium ships a header-only amalgamation: the whole HTTP server is
# the single file single_headers/lithium_http_server.hh. There is NO
# system package / find_package for it (unlike drogon), so we vendor the
# header at configure time. Resolution order:
#
#   1. LITHIUM_HEADER_DIR (cache/env) — a directory already containing
#      lithium_http_server.hh. The cpp ansible role points this at the
#      bench-staged copy it fetched once (pristine: no network at adapter
#      build time, header lives under bench_root).
#   2. ${CMAKE_CURRENT_SOURCE_DIR}/third_party — an in-tree vendored copy.
#   3. file(DOWNLOAD ...) of a PINNED upstream commit into the build tree
#      (local-dev / CI fallback; requires network at configure time).
#
# LITHIUM_COMMIT pins the upstream revision so the build is reproducible —
# bump it deliberately, never float on master. The pin below is the
# revision this adapter's API was written against (li::http_api,
# response.set_header/write, request.url_parameters(s::id=...),
# request.http_ctx.read_whole_body(), li::http_serve(api, port,
# s::nthreads=...)).
# Pinned (not "master") so the input-buffer patch below targets an exact,
# reproducible source literal. This tip still has `buffer_(50 * 1024)` verbatim
# and an unchanged adapter API (li::http_api, read_whole_body, s::nthreads).
set(LITHIUM_COMMIT "879620e6df4540eefa4c6ae408934307d305e9d9" CACHE STRING "Upstream matt-42/lithium revision to vendor")

set(_lithium_header "")
if(DEFINED LITHIUM_HEADER_DIR AND EXISTS "${LITHIUM_HEADER_DIR}/lithium_http_server.hh")
    set(_lithium_header "${LITHIUM_HEADER_DIR}")
elseif(DEFINED ENV{LITHIUM_HEADER_DIR} AND EXISTS "$ENV{LITHIUM_HEADER_DIR}/lithium_http_server.hh")
    set(_lithium_header "$ENV{LITHIUM_HEADER_DIR}")
elseif(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/third_party/lithium_http_server.hh")
    set(_lithium_header "${CMAKE_CURRENT_SOURCE_DIR}/third_party")
else()
    set(_lithium_header "${CMAKE_CURRENT_BINARY_DIR}/lithium")
    if(NOT EXISTS "${_lithium_header}/lithium_http_server.hh")
        message(STATUS "lithium: downloading single header (commit ${LITHIUM_COMMIT})")
        file(DOWNLOAD
            "https://raw.githubusercontent.com/matt-42/lithium/${LITHIUM_COMMIT}/single_headers/lithium_http_server.hh"
            "${_lithium_header}/lithium_http_server.hh"
            TLS_VERIFY ON
            STATUS _dl_status
            SHOW_PROGRESS)
        list(GET _dl_status 0 _dl_code)
        if(NOT _dl_code EQUAL 0)
            list(GET _dl_status 1 _dl_msg)
            message(FATAL_ERROR "lithium: failed to download single header: ${_dl_msg}. "
                                "Set -DLITHIUM_HEADER_DIR=<dir containing lithium_http_server.hh> "
                                "or vendor it at third_party/lithium_http_server.hh.")
        endif()
    endif()
endif()
message(STATUS "lithium: using header from ${_lithium_header}")

# --- patch the input buffer cap (post-64k / post-1m DNF fix) ---------------
#
# lithium's single header hard-codes the per-connection input buffer at
# `buffer_(50 * 1024)` (50 KiB) and NEVER grows it, so a request body larger
# than ~50 KiB (post-64k = 64 KiB, post-1m = 1 MiB) yields zero successful
# requests (the server reads-and-discards via read_whole_body, but the body
# never fully buffers). Copy the resolved header into a build-owned dir and
# bump the cap to 2 MiB, then HARD-FAIL if the literal wasn't found (so a
# silent upstream drift can't ship the 50 KiB cap with a green build).
set(_lithium_patched "${CMAKE_CURRENT_BINARY_DIR}/lithium_patched")
configure_file("${_lithium_header}/lithium_http_server.hh"
               "${_lithium_patched}/lithium_http_server.hh" COPYONLY)
execute_process(
    COMMAND sed -i.bak "s/buffer_(50 \\* 1024)/buffer_(2 * 1024 * 1024)/"
            "${_lithium_patched}/lithium_http_server.hh"
    RESULT_VARIABLE _sed_rc)
file(READ "${_lithium_patched}/lithium_http_server.hh" _patched_src)
string(FIND "${_patched_src}" "buffer_(2 * 1024 * 1024)" _has_patch)
if(_has_patch EQUAL -1)
    message(FATAL_ERROR "lithium: input-buffer patch did not apply — upstream literal "
                        "'buffer_(50 * 1024)' not found at commit ${LITHIUM_COMMIT}; refusing to "
                        "build the 50KB-capped header (would dnf post-64k/post-1m). Re-pin "
                        "LITHIUM_COMMIT and update the sed pattern.")
endif()
message(STATUS "lithium: input buffer patched to 2 MiB")

# --- link dependencies ----------------------------------------------------
#
# lithium links against pthread (Threads), OpenSSL (ssl + crypto, used by
# the HTTP server even without TLS configured), and Boost.Context (its
# fiber-per-connection reactor). These mirror the TechEmpower lithium
# compile.sh: -lpthread -lboost_context -lssl -lcrypto.
find_package(Threads REQUIRED)
find_package(OpenSSL REQUIRED)
find_package(Boost REQUIRED COMPONENTS context)

add_executable(lithium-adapter src/main.cc)
target_include_directories(lithium-adapter PRIVATE "${_lithium_patched}")
target_link_libraries(lithium-adapter PRIVATE
    Threads::Threads
    OpenSSL::SSL
    OpenSSL::Crypto
    Boost::context)

# Bench-grade optimization, matching the TechEmpower lithium build:
#   -O3 -march=native -flto, NDEBUG, and LITHIUM_SERVER_NAME shortens the
#   emitted Server: header. The cpp ansible role also exports CXXFLAGS=
#   -march=native; setting it here too keeps a standalone `cmake --build`
#   identically optimized.
target_compile_definitions(lithium-adapter PRIVATE NDEBUG LITHIUM_SERVER_NAME=l)
target_compile_options(lithium-adapter PRIVATE -O3 -march=native -flto)
set_target_properties(lithium-adapter PROPERTIES
    INTERPROCEDURAL_OPTIMIZATION TRUE
    LINK_FLAGS "-flto")
