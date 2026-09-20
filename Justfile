# This file is the project's own — add recipes below. Keep the import: it
# mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# Re-exports main.just's PATH with build/erofs-utils/bin (where
# build-erofs-utils.sh installs mkfs.erofs) in front; the importing file's
# definition wins. Keep the rest identical to main.just's — no /usr/local, no
# homebrew.
export PATH := if os() == 'windows' { justfile_directory() / 'build' / 'erofs-utils' / 'bin' + ';' + aqua_bin + ';' + env_var('PATH') } else { justfile_directory() / 'build' / 'erofs-utils' / 'bin' + ':' + aqua_bin + ':/usr/bin:/bin:/usr/sbin:/sbin' }

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::go::bce do::lint::go::escape do::lint::go::deadcode do::lint::default
fix: do::fix::go::default do::fix::default
test: mkfs-info do::test::go::unit do::test::go::race
bench: do::test::go::bench

# Image-backed tests skip themselves without mkfs.erofs, so a green run proves
# less than it looks. Locally: `.github/scripts/build-erofs-utils.sh native`
# puts it where this looks.
[doc('Report whether mkfs.erofs is available to the image-backed tests (EROFS_REQUIRE_MKFS=1 to fail if not)')]
mkfs-info:
    #!/usr/bin/env bash
    set -euo pipefail
    if command -v mkfs.erofs > /dev/null 2>&1; then
        echo "mkfs.erofs: $(mkfs.erofs -V 2>&1 | head -n 1) — image-backed tests will run"
        echo "  at $(command -v mkfs.erofs)"
    elif [ "${EROFS_REQUIRE_MKFS:-}" = "1" ]; then
        echo "mkfs.erofs: NOT FOUND, and EROFS_REQUIRE_MKFS=1 — this leg was supposed to have it (build/erofs-utils/bin)" >&2
        exit 1
    else
        echo "mkfs.erofs: NOT FOUND — every image-backed test will skip itself"
    fi

# Recompute the sha256 of each source tarball pinned in build-erofs-utils.sh and
# rewrite the script. Run after a version bump (Renovate's or yours), then commit.
# GitHub publishes no digest for an archive tarball, so the hash is of the
# download itself; the pin then guards every later fetch against a moved tag.
[doc('Refresh the erofs-utils and lz4 tarball sha256 pins in build-erofs-utils.sh')]
refresh-pins:
    #!/usr/bin/env bash
    set -euo pipefail
    script=.github/scripts/build-erofs-utils.sh
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    # pin <VAR> <url>: download, hash, rewrite the `VAR_SHA256="…"` line.
    pin() {
        local file
        file="$tmp/$(basename "$2")"
        curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 --retry-all-errors -o "$file" "$2"
        local sum
        sum="$(sha256sum "$file" | cut -d' ' -f1)"
        grep -qE "^$1_SHA256=\"[0-9a-f]{64}\"$" "$script" || { echo "refresh-pins: no $1_SHA256 line in $script" >&2; exit 1; }
        sed -i.bak -E "s|^($1_SHA256=\")[0-9a-f]{64}(\")$|\1$sum\2|" "$script" && rm "$script.bak"
        echo ">> $1_SHA256 = $sum"
    }
    version() { grep -oE "^$1_VERSION=\"[^\"]+\"" "$script" | cut -d'"' -f2; }
    pin EROFS_UTILS "https://github.com/erofs/erofs-utils/archive/refs/tags/v$(version EROFS_UTILS).tar.gz"
    pin LZ4 "https://github.com/lz4/lz4/archive/refs/tags/v$(version LZ4).tar.gz"
