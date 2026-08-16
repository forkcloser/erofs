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

# Fuzz smoke: every Fuzz* target, each for a short budget — a regression net,
# not a campaign. TEMPORARY, and verbatim what limen's `do::test::go::fuzz`
# will be once a release ships it; the day `just do test go fuzz` exists, this
# recipe is deleted and CI's fuzz job calls that instead. Not part of `test`:
# it needs mkfs.erofs on PATH to be worth anything (see mkfs-info) and runs
# once, on linux, in CI.
#
# Driven by `go test -fuzz` itself, never a prebuilt test binary: only the go
# tool's fuzz build compiles in the coverage counters, and without them the
# engine mutates blind. The verdict comes from the crasher, not the exit code:
# a real finding always writes the failing input under testdata/fuzz/<Target>/
# (commit it — it is a regression test from then on), while the coordinator
# can also exit 1 with "context deadline exceeded" when a worker is
# mid-iteration as fuzztime expires — a shutdown hiccup, not a finding, and
# the Wide targets hit it most. So exit 1 WITH a new testdata file fails; exit
# 1 without one is retried once, and a second in a row is treated as real. The
# generated corpus lives under $(go env GOCACHE)/fuzz — CI caches it between
# runs so fuzzing is cumulative.
[doc('Run every Fuzz* target for TEST_GO_FUZZ_TIME (default 10s) each')]
fuzz:
    #!/usr/bin/env bash
    set -euo pipefail
    fuzztime="${TEST_GO_FUZZ_TIME:-10s}"
    # Crasher count for a target: the directory does not exist until the first
    # finding, and a missing directory is zero, not an error (pipefail).
    crashers() {
        if [ -d "$1" ]; then find "$1" -type f | wc -l; else echo 0; fi
    }
    found=0
    fail=0
    for pkg in $(go list ./...); do
        targets=$(go test -count=1 -run '^$' -list '^Fuzz' "$pkg" | grep '^Fuzz' || true)
        [ -n "$targets" ] || continue
        dir=$(go list -f '{{{{.Dir}}' "$pkg")
        for target in $targets; do
            found=1
            echo "fuzzing $pkg $target for $fuzztime"
            before=$(crashers "$dir/testdata/fuzz/$target")
            ok=0
            for attempt in 1 2; do
                if go test -count=1 -timeout "${TEST_GO_TIMEOUT:-10m}" \
                    -run "^${target}\$" -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg"; then
                    ok=1
                    break
                fi
                after=$(crashers "$dir/testdata/fuzz/$target")
                if [ "$after" -gt "$before" ]; then
                    echo "$target: new crasher written under $dir/testdata/fuzz/$target — commit it as a regression test" >&2
                    break
                fi
                echo "$target: exit without a crasher (attempt $attempt) — coordinator shutdown hiccup, retrying" >&2
            done
            if [ "$ok" -eq 1 ]; then
                echo "PASS: $target"
            else
                fail=1
            fi
        done
    done
    if [ "$found" -eq 0 ]; then
        echo "no Fuzz* targets in this module — nothing to fuzz"
    fi
    exit "$fail"
