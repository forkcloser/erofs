# This file is the project's own — add recipes below. Keep the import: it
# mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# Re-exports main.just's PATH with .build/mkfs-erofs/bin in front, where the
# mkfs-erofs recipe puts the mkfs.erofs the image-backed tests run; the
# importing file's definition wins. Keep the rest identical to main.just's —
# no /usr/local, no homebrew.
export PATH := if os() == 'windows' { justfile_directory() / '.build' / 'mkfs-erofs' / 'bin' + ';' + hermetic_bin + ';' + env_var('PATH') } else { justfile_directory() / '.build' / 'mkfs-erofs' / 'bin' + ':' + hermetic_bin + ':/usr/bin:/bin:/usr/sbin:/sbin' }

# mkfs.erofs makes its scratch files in $TMPDIR; windows does not set it, and
# a native binary there needs a windows path, which TEMP is.
export TMPDIR := env_var_or_default('TMPDIR', env_var_or_default('TEMP', '/tmp'))

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::go::default do::lint::go::deadcode do::lint::default
fix: do::fix::go::default do::fix::default
test: mkfs-erofs do::test::go::unit do::test::go::race
bench: do::perf::go::bench

# The mkfs.erofs the image-backed tests run, and the fsck.erofs that reads
# their images back, from the archive pins.yaml pins
# for this host, verified against the pinned sha256 on every run and unpacked
# into .build/mkfs-erofs/bin. Network on first run. The image-backed tests
# skip themselves without it, so a green run without this recipe proves less
# than it looks; `just test` runs it first.
[doc('Fetch the pinned mkfs.erofs for this host into .build/mkfs-erofs/bin')]
mkfs-erofs:
    #!/usr/bin/env bash
    set -euo pipefail
    pin="mkfs-erofs-$(go env GOOS)-$(go env GOARCH)"
    url=$(limen pins get "${pin}" url)
    sha256=$(limen pins get "${pin}" sha256)
    x=""
    [ "$(go env GOOS)" = windows ] && x=".exe"
    mkdir -p .build/mkfs-erofs/bin
    archive=".build/mkfs-erofs/${url##*/}"
    if [ ! -f "${archive}" ] || ! echo "${sha256}  ${archive}" | sha256sum -c --status; then
        curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-delay 3 -o "${archive}" "${url}"
        echo "${sha256}  ${archive}" | sha256sum -c --status
    fi
    name=${archive##*/}
    name=${name%.tar.gz}
    rm -rf ".build/mkfs-erofs/${name}"
    tar -xzf "${archive}" -C .build/mkfs-erofs
    mv -f ".build/mkfs-erofs/${name}/mkfs.erofs${x}" ".build/mkfs-erofs/bin/mkfs.erofs${x}"
    # fsck.erofs is in the archive where it builds (not windows): the tests
    # hold their images to its verdict where it is on PATH.
    rm -f .build/mkfs-erofs/bin/fsck.erofs
    [ ! -e ".build/mkfs-erofs/${name}/fsck.erofs" ] || mv -f ".build/mkfs-erofs/${name}/fsck.erofs" .build/mkfs-erofs/bin/
    echo "mkfs.erofs: $(".build/mkfs-erofs/bin/mkfs.erofs${x}" -V 2>&1 | head -n 1) at .build/mkfs-erofs/bin"

# --- added by limen fix: the recipe the security workflow runs ---
security: do::security::default
