#!/usr/bin/env bash
# Build erofs-utils from source, patched, for the image-backed tests.
#
# INTERIM. This script exists because erofs-utils has no release binary aqua
# could pin: it is a C project distributed as source, so the pinned toolchain
# cannot carry an mkfs.erofs and every image-backed test skips itself without
# one. The end state is a forkcloser release of erofs-utils (the patches this
# script applies are already ours) pinned in aqua.yaml like any other tool, at
# which point this script and its CI steps go away and `just test` alone is
# the whole story.
#
# Until then, the doctrine ci.yaml states is honored as far as source builds
# allow: exact versions, sha256-verified downloads, and one implementation
# shared by every job that needs it (a divergence between two copies of this
# was the classic failure of the workflow this replaced). What stays
# unpinned, knowingly: the build-dependency packages from the runner's own
# apt/brew repositories.
#
# Where it lands — and why that matters: `just` runs every recipe under limen's
# HERMETIC PATH (aqua's bin plus the base system dirs; see .limen/just/main.just),
# so a `make install` into /usr/local is invisible to `just test` and every
# image-backed test skips itself while the log says the build succeeded. The
# binary therefore installs into the project's own tool dir, build/erofs-utils/,
# whose bin/ the root Justfile prepends to PATH — the one declared exception to
# the hermetic list, and the same location on every OS (the windows cross-build
# is copied there by CI). No sudo, no system prefix.
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

EROFS_UTILS_VERSION="1.9.3"
EROFS_UTILS_SHA256="17bfa54f4d370838c61081fce44022815a0366e282d777389589184414d5adc5"
LZ4_VERSION="1.10.0"
LZ4_SHA256="537512904744b35e232912055ccf8ec66d768639ff3abe5788d90d792ec5f48b"
MINGW_HOST="x86_64-w64-mingw32"

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
patches="$repo/.github/workflows/patches/erofs-utils"
headers="$repo/.github/workflows/mingw-compat-headers"
work="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/erofs-utils-build"
prefix="$repo/build/erofs-utils"

die() {
    echo "build-erofs-utils: $*" >&2
    exit 1
}

# fetch <url> <sha256> <dest>: download and verify, or fail closed.
fetch() {
    local url=$1 sum=$2 dest=$3 actual
    curl -fsSL -o "$dest" "$url"
    if command -v sha256sum > /dev/null 2>&1; then
        actual=$(sha256sum "$dest" | awk '{print $1}')
    else
        actual=$(shasum -a 256 "$dest" | awk '{print $1}')
    fi
    [ "$actual" = "$sum" ] || die "checksum mismatch for $url: want $sum, got $actual"
}

# unpack_erofs_utils: fetch, verify, extract, and patch the source into $work;
# prints the source directory.
unpack_erofs_utils() {
    local tarball="$work/erofs-utils-${EROFS_UTILS_VERSION}.tar.gz"
    local src="$work/erofs-utils-${EROFS_UTILS_VERSION}"
    mkdir -p "$work"
    rm -rf "$src"
    fetch "https://github.com/erofs/erofs-utils/archive/refs/tags/v${EROFS_UTILS_VERSION}.tar.gz" \
        "$EROFS_UTILS_SHA256" "$tarball"
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
                sudo apt-get update -qq
                sudo apt-get install -y -qq autoconf automake libtool pkg-config libz-dev liblz4-dev uuid-dev
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
        sudo apt-get update -qq
        sudo apt-get install -y -qq autoconf automake libtool pkg-config mingw-w64
    fi
    mkdir -p "$work"

    # lz4, static, into the mingw sysroot — the only library the cross build
    # links; everything else is configured out below.
    local lz4_tarball="$work/lz4-${LZ4_VERSION}.tar.gz"
    fetch "https://github.com/lz4/lz4/archive/refs/tags/v${LZ4_VERSION}.tar.gz" "$LZ4_SHA256" "$lz4_tarball"
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
