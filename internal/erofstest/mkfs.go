package erofstest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CheckMkfsVersion checks that mkfs.erofs is at least the given minimum
// version. It returns nil if the version is sufficient, or an error describing
// the mismatch. Version strings are compared using major.minor.patch semantics.
func CheckMkfsVersion(ctx context.Context, minimum string) (bool, error) {
	ver, err := mkfsVersion(ctx)
	if err != nil {
		return false, err
	}

	cur, err := parseVersion(ver)
	if err != nil {
		return false, fmt.Errorf("parsing mkfs.erofs version %q: %w", ver, err)
	}

	minVer, err := parseVersion(minimum)
	if err != nil {
		return false, fmt.Errorf("parsing minimum version %q: %w", minimum, err)
	}

	return cur.less(minVer), nil
}

// errVersionOutput is `mkfs.erofs -V` printing something other than
// "mkfs.erofs (erofs-utils) <version>" on its first line.
var errVersionOutput = errors.New("unexpected mkfs.erofs version output")

type semver struct {
	major, minor, patch int
}

func (v semver) less(other semver) bool {
	if v.major != other.major {
		return v.major < other.major
	}

	if v.minor != other.minor {
		return v.minor < other.minor
	}

	return v.patch < other.patch
}

func parseVersion(s string) (semver, error) {
	parts := strings.SplitN(s, ".", 3)

	var (
		v   semver
		err error
	)

	v.major, err = strconv.Atoi(parts[0])
	if err != nil {
		return v, fmt.Errorf("invalid major version: %w", err)
	}

	if len(parts) >= 2 {
		v.minor, err = strconv.Atoi(parts[1])
		if err != nil {
			return v, fmt.Errorf("invalid minor version: %w", err)
		}
	}

	if len(parts) >= 3 {
		v.patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return v, fmt.Errorf("invalid patch version: %w", err)
		}
	}

	return v, nil
}

func mkfsVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "mkfs.erofs", "-V").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mkfs.erofs -V failed: %s: %w", out, err)
	}
	// Output format: "mkfs.erofs (erofs-utils) 1.9.1\n..."
	line, _, _ := strings.Cut(string(out), "\n")

	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", fmt.Errorf("%w: %q", errVersionOutput, line)
	}

	return strings.TrimPrefix(fields[len(fields)-1], "v"), nil
}
