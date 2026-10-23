#!/usr/bin/env bash
# Build erofs-utils from source, patched, for the image-backed tests.
#
# Installs into build/erofs-utils/ — the root .justfile prepends its bin/ to
# limen's hermetic PATH; a /usr/local install is invisible to `just test`. The
# apt/brew build-dependency packages are the one unpinned input. INTERIM until
# a forkcloser erofs-utils release is pinned in .hermetic/hermetic.yaml.
#
# Usage:
#   build-erofs-utils.sh native    build and install for this host (linux or
#                                  macOS) into build/erofs-utils/bin/mkfs.erofs
#   build-erofs-utils.sh windows   cross-compile a static mkfs.erofs.exe with
#                                  MinGW-w64 (linux host) into
#                                  build/erofs-utils/bin/mkfs.erofs.exe
# Set SKIP_DEPS=1 to skip the apt/brew build-dependency step (a developer
# machine that already has autotools and lz4).
set -euo pipefail

# The build wants the runner's coreutils, autotools and compiler, not the
# pinned tools: hermetic's links do not resolve outside the checkout, where the
# build runs. limen, for pin below, still resolves through hermetic's bin
# further down.
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

MINGW_HOST="x86_64-w64-mingw32"

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
patches="$repo/.github/scripts/patches/erofs-utils"
headers="$repo/.github/scripts/mingw-compat-headers"
work="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/erofs-utils-build"
prefix="$repo/build/erofs-utils"

# Both tarballs are pinned in pins.yaml at the repository root: Renovate moves
# the version, limen refreshes the digest, and this script only reads them back.
pin() {
    (cd "$repo" && limen pins get "$@")
}
EROFS_UTILS_VERSION="$(pin erofs-utils version)"
EROFS_UTILS_URL="$(pin erofs-utils url)"
EROFS_UTILS_SHA256="$(pin erofs-utils sha256)"
LZ4_VERSION="$(pin lz4 version)"
LZ4_URL="$(pin lz4 url)"
LZ4_SHA256="$(pin lz4 sha256)"

die() {
    echo "build-erofs-utils: $*" >&2
    exit 1
}

# apt is the one input the script cannot pin, and the one that has stalled: a
# silent twenty minutes under -qq, the job's whole budget, with nothing in the
# log to say whether the index refresh or the mingw download was the part not
# moving. So each call is bounded and talks: a line names the phase before it
# runs, -q keeps the Get: lines that show a download progressing, Acquire's
# retries and per-connection timeout turn a dead mirror into a quick failure,
# and timeout(1) caps each call at five minutes, ten for the two together
# under the job's twenty, with a kill thirty seconds after the term for an
# apt-get deaf to it, so a stall fails here, with the phase in the log,
# rather than as a cancelled job. The frontend is pinned so no package can
# wait on a question nobody is there to answer.
apt_get() {
    echo "build-erofs-utils: apt-get $1" >&2
    sudo timeout --kill-after=30 300 env DEBIAN_FRONTEND=noninteractive apt-get -q \
        -o Acquire::Retries=3 \
        -o Acquire::http::Timeout=30 \
        -o Acquire::https::Timeout=30 \
        -o Dpkg::Use-Pty=0 \
        "$@" || die "apt-get $1 failed or stalled past five minutes (exit $?)"
}

# fetch <url> <sha256> <dest>: download and verify, or fail closed. The
# download is the org's canonical curl shape: https only, TLS 1.2 floor,
# retries on transient failures; the sha256 below is the real guard.
fetch() {
    local url=$1 sum=$2 dest=$3 actual
    curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors -o "$dest" "$url"
        actual=$(sha256sum "$dest" | awk '{print $1}')
    [ "$actual" = "$sum" ] || die "checksum mismatch for $url: want $sum, got $actual"
}

# unpack_erofs_utils: fetch, verify, extract, and patch the source into $work;
# prints the source directory.
unpack_erofs_utils() {
    local tarball="$work/erofs-utils-${EROFS_UTILS_VERSION}.tar.gz"
    local src="$work/erofs-utils-${EROFS_UTILS_VERSION}"
    mkdir -p "$work"
    rm -rf "$src"
    fetch "$EROFS_UTILS_URL" "$EROFS_UTILS_SHA256" "$tarball"
    tar -xzf "$tarball" -C "$work"
    local p
    for p in "$patches"/*.patch; do
        patch -d "$src" -p1 < "$p" > /dev/null
    done
    echo "$src"
}

nproc_portable() {
    nproc 2> /dev/null || sysctl -n hw.ncpu
}

build_native() {
    if [ -z "${SKIP_DEPS:-}" ]; then
        case "$(uname -s)" in
            Linux)
                apt_get update
                apt_get install -y autoconf automake libtool pkg-config libz-dev liblz4-dev uuid-dev
                ;;
            Darwin)
                brew install autoconf automake libtool pkg-config lz4
                ;;
            *) die "native build supports linux and macOS only (got $(uname -s))" ;;
        esac
    fi
    local src
    src=$(unpack_erofs_utils)
    (
        cd "$src"
        ./autogen.sh
        # configure caps the block size at the BUILD host's page size (bumped
        # to 16K only when the build CPU is aarch64), so the same source
        # yields a different mkfs per runner. Pin it: the 16384 leg of
        # TestReadReferenceImage skips itself otherwise.
        MAX_BLOCK_SIZE=16384 ./configure --enable-lz4 --prefix="$prefix"
        make -j"$(nproc_portable)"
        make install
    )
    "$prefix/bin/mkfs.erofs" -V
    echo "installed $prefix/bin/mkfs.erofs"
}

build_windows() {
    [ "$(uname -s)" = "Linux" ] || die "the windows cross-build needs a linux host"
    if [ -z "${SKIP_DEPS:-}" ]; then
        apt_get update
        apt_get install -y autoconf automake libtool pkg-config mingw-w64
    fi
    mkdir -p "$work"

    # lz4, static, into the mingw sysroot — the only library the cross build
    # links; everything else is configured out below.
    local lz4_tarball="$work/lz4-${LZ4_VERSION}.tar.gz"
    fetch "$LZ4_URL" "$LZ4_SHA256" "$lz4_tarball"
    rm -rf "$work/lz4-${LZ4_VERSION}"
    tar -xzf "$lz4_tarball" -C "$work"
    (
        cd "$work/lz4-${LZ4_VERSION}/lib"
        make -j"$(nproc_portable)" \
            CC="${MINGW_HOST}-gcc" AR="${MINGW_HOST}-ar" WINDRES="${MINGW_HOST}-windres" \
            TARGET_OS=Windows BUILD_STATIC=yes BUILD_SHARED=no \
            CFLAGS="-O3 -DXXH_NAMESPACE=LZ4_"
        sudo make PREFIX="/usr/${MINGW_HOST}" install
    )

    local src
    src=$(unpack_erofs_utils)
    # The compat headers stand in for the POSIX surface MinGW lacks; they are
    # force-included into every translation unit below.
    sudo cp -r "$headers"/* "/usr/${MINGW_HOST}/include/"
    (
        cd "$src"
        ./autogen.sh
        # PKG_CONFIG_LIBDIR (not _PATH): _PATH prepends to the host's search
        # dirs, so host .pc files leak into the cross build — v1.9.3's libxml2
        # auto-probe found the runner's libxml-2.0.pc and put -lxml2 on a link
        # line no mingw library can satisfy. _LIBDIR replaces the search path
        # outright: only the mingw sysroot (where the cross-compiled lz4
        # installs its .pc) is visible, and every other auto-probe fails closed.
        # MAX_BLOCK_SIZE: same pin as the native build, same reason.
        PKG_CONFIG_LIBDIR="/usr/${MINGW_HOST}/lib/pkgconfig" \
            MAX_BLOCK_SIZE=16384 \
            ./configure \
            --host="${MINGW_HOST}" \
            --disable-shared \
            --enable-lz4 \
            --without-zlib \
            --disable-lzma \
            --without-libzstd \
            --without-selinux \
            --without-uuid \
            --without-openssl \
            --without-libxml2 \
            --disable-fuse \
            --disable-debug \
            --disable-dependency-tracking \
            CFLAGS="-O2 -g -D_FILE_OFFSET_BITS=64" \
            LDFLAGS="-Wl,-Bstatic -static-libgcc -L/usr/${MINGW_HOST}/lib" \
            liblz4_LIBS="/usr/${MINGW_HOST}/lib/liblz4.a"
        make -j"$(nproc_portable)" -C lib CPPFLAGS="-D_GNU_SOURCE -include posix_compat.h"
        make -j"$(nproc_portable)" -C mkfs CPPFLAGS="-D_GNU_SOURCE -include posix_compat.h" LIBS="-llz4"
        "${MINGW_HOST}-strip" mkfs/mkfs.erofs.exe
    )
    mkdir -p "$prefix/bin"
    cp "$src/mkfs/mkfs.erofs.exe" "$prefix/bin/mkfs.erofs.exe"
    echo "installed $prefix/bin/mkfs.erofs.exe"
}

case "${1:-}" in
    native) build_native ;;
    windows) build_windows ;;
    *) die "usage: $0 native|windows" ;;
esac
