# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::default
fix: do::fix::go::default do::fix::default
test: mkfs-info do::test::go::unit do::test::go::race
bench: do::test::go::bench

# The tests that read a real image shell out to mkfs.erofs and skip themselves
# when it is absent, so a run without erofs-utils silently covers far less than
# one with it. Say which it was: a green run that skipped every image test is
# not evidence about the image reader. Diagnostic only — never fails.
[doc('Report whether mkfs.erofs is available to the image-backed tests')]
mkfs-info:
    #!/usr/bin/env bash
    set -euo pipefail
    if command -v mkfs.erofs > /dev/null 2>&1; then
        echo "mkfs.erofs: $(mkfs.erofs -V 2>&1 | head -n 1) — image-backed tests will run"
    else
        echo "mkfs.erofs: NOT FOUND — every image-backed test will skip itself"
    fi
