# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# One declared exception to limen's hermetic PATH: build/erofs-utils/bin, where
# .github/scripts/build-erofs-utils.sh installs mkfs.erofs (a C tool with no
# release binary aqua could pin, so the tests that read real images cannot get
# it any other way). Same expression as main.just's, with that one directory in
# front — a project's root Justfile may re-export a shared variable, and the
# importing file wins. Nothing else is added: /usr/local and homebrew stay out.
export PATH := if os() == 'windows' { justfile_directory() / 'build' / 'erofs-utils' / 'bin' + ';' + aqua_bin + ';' + env_var('PATH') } else { justfile_directory() / 'build' / 'erofs-utils' / 'bin' + ':' + aqua_bin + ':/usr/bin:/bin:/usr/sbin:/sbin' }

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::default
fix: do::fix::go::default do::fix::default
test: mkfs-info do::test::go::unit do::test::go::race
bench: do::test::go::bench

# The tests that read a real image shell out to mkfs.erofs and skip themselves
# when it is absent, so a run without erofs-utils silently covers far less than
# one with it. Say which it was: a green run that skipped every image test is
# not evidence about the image reader. Diagnostic by default; with
# EROFS_REQUIRE_MKFS=1 (set by every CI leg that just built mkfs.erofs) a
# missing binary FAILS the run — a coverage regression must be red, not a line
# in a log. Locally: `.github/scripts/build-erofs-utils.sh native` (SKIP_DEPS=1
# if you already have autotools and lz4) puts it where this looks.
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
