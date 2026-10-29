#!/usr/bin/env bash
# Zig CC wrapper for cross-compiling Denyzer with CGO enabled.
#
# Denyzer stores its query log in SQLite through the native driver
# (github.com/mattn/go-sqlite3), so every release binary has to be built with
# CGO_ENABLED=1. Go silently turns cgo off as soon as GOOS/GOARCH differ from
# the host, and no system C compiler can target every platform Denyzer ships.
# The Zig compiler bundles a libc for all of them, so this wrapper maps the Go
# target onto a Zig target triple and forwards the compiler invocation to
# `zig cc`.
#
# The wrapper:
#   1. maps GOOS/GOARCH/GOARM/GOMIPS onto a Zig target triple,
#   2. drops linker flags that LLD rejects for the BSD targets,
#   3. answers the GNU ld probe Go runs on some targets,
#   4. adds -Wno-error=option-ignored so Zig's "unused option" diagnostics do
#      not fail the build (Go passes -fno-PIC, which Zig cannot honour on the
#      mips64 N64 ABI).
#
# macOS is deliberately not handled here: Go's darwin runtime always links
# -lresolv and imports res_9_* from /usr/lib/libresolv.9.dylib, which only
# exists on a real macOS host. macOS binaries are therefore built on a macOS
# runner with the native Apple compiler (see .goreleaser.yml).
#
# Usage:
#   CC=$PWD/scripts/zigcc/cc-zig.sh CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build
#
# Environment:
#   ZIG     path to the zig binary (default: first zig in PATH)
set -euo pipefail

ZIG="${ZIG:-}"
if [ -z "$ZIG" ]; then
  ZIG="$(command -v zig || true)"
fi
if [ -z "$ZIG" ]; then
  echo "cc-zig.sh: zig not found; install Zig or point ZIG at the compiler" >&2
  exit 1
fi

GOOS="${GOOS:-$(go env GOOS)}"
GOARCH="${GOARCH:-$(go env GOARCH)}"
GOARM="${GOARM:-$(go env GOARM)}"
GOMIPS="${GOMIPS:-$(go env GOMIPS)}"
GOMIPS64="${GOMIPS64:-$(go env GOMIPS64)}"

case "${GOOS}/${GOARCH}" in
  linux/amd64) target=x86_64-linux-musl ;;
  linux/arm64) target=aarch64-linux-musl ;;
  linux/arm)
    # GOARM=5/6 use the soft-float ABI, GOARM=7 the hard-float one. The ABI
    # has to match the libc, otherwise floats are passed in different registers
    # on the C and the Go side of the cgo boundary.
    case "$GOARM" in
      5 | 6) target=arm-linux-musleabi ;;
      *) target=arm-linux-musleabihf ;;
    esac
    ;;
  linux/mips)
    if [ "$GOMIPS" = "hardfloat" ]; then target=mips-linux-musleabihf; else target=mips-linux-musleabi; fi
    ;;
  linux/mipsle)
    if [ "$GOMIPS" = "hardfloat" ]; then target=mipsel-linux-musleabihf; else target=mipsel-linux-musleabi; fi
    ;;
  linux/mips64)
    # Zig only ships the soft-float N64 ABI for mips64.
    target=mips64-linux-muslabi64
    ;;
  linux/mips64le)
    target=mips64el-linux-muslabi64
    ;;
  windows/amd64) target=x86_64-windows-gnu ;;
  freebsd/amd64) target=x86_64-freebsd-none ;;
  freebsd/arm) target=arm-freebsd-eabihf ;;
  freebsd/arm64) target=aarch64-freebsd-none ;;
  netbsd/amd64) target=x86_64-netbsd-none ;;
  netbsd/arm) target=arm-netbsd-eabihf ;;
  netbsd/arm64) target=aarch64-netbsd-none ;;
  openbsd/amd64) target=x86_64-openbsd-none ;;
  openbsd/arm) target=arm-openbsd-eabi ;;
  openbsd/arm64) target=aarch64-openbsd-none ;;
  darwin/*)
    echo "cc-zig.sh: macOS targets need the macOS system libraries and must be" >&2
    echo "cc-zig.sh: built on a macOS runner, see .goreleaser.yml" >&2
    exit 1
    ;;
  *)
    echo "cc-zig.sh: unsupported target ${GOOS}/${GOARCH}" >&2
    exit 1
    ;;
esac

# Go probes the external linker with "-fuse-ld=bfd -Wl,--version" on some
# targets and requires the output to contain "GNU ld". Zig ships LLD only, so
# answer the probe with a GNU ld version string and drop the -fuse-ld=bfd flag
# from the real link.
probe=0
for a in "$@"; do
  if [ "$a" = "-Wl,--version" ]; then
    probe=1
  fi
done

if [ "$probe" = 1 ]; then
  echo "GNU ld (GNU Binutils) 2.43"
  exit 0
fi

args=()
for a in "$@"; do
  case "$a" in
    # LLD does not accept these for the BSD targets Go builds for.
    -Wl,-nopie) ;;
    -Wl,--no-execute-only) ;;
    -fuse-ld=bfd) ;;
    *) args+=("$a") ;;
  esac
done

exec "$ZIG" cc -target "$target" -Wno-error=option-ignored "${args[@]}"
