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
