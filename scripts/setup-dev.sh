#!/usr/bin/env bash
# Set up a development machine for this repository: the linter CI pins, and the
# pre-push hook that runs it. Safe to re-run; it changes only what is wrong.
#
# The linter version is read from .github/workflows/ci.yml rather than repeated
# here, so the two cannot drift apart and lint locally on a version CI does not
# use.
#
#   ./scripts/setup-dev.sh          install what is missing
#   ./scripts/setup-dev.sh --check  report only, change nothing (exit 1 if work is due)
set -uo pipefail

cd "$(git rev-parse --show-toplevel)" || exit 1

check_only=0
[ "${1:-}" = "--check" ] && check_only=1

workflow=.github/workflows/ci.yml
note()  { printf '  %s\n' "$*"; }
fail()  { printf '\n%s\n' "$*" >&2; exit 1; }

# --- the linter version CI pins -------------------------------------------

# The workflow also carries `go-version:` keys, so match only a value shaped
# like a semver tag.
pinned=$(grep -oE '^[[:space:]]+version:[[:space:]]*v[0-9]+\.[0-9]+\.[0-9]+' "$workflow" 2>/dev/null | grep -oE 'v[0-9.]+$')
[ -n "$pinned" ] || fail "Could not read the golangci-lint version from $workflow."

echo "golangci-lint (CI pins $pinned)"

installed=""
if command -v golangci-lint >/dev/null 2>&1; then
	installed=$(golangci-lint --version 2>/dev/null | grep -oE 'version v?[0-9]+\.[0-9]+\.[0-9]+' | grep -oE '[0-9.]+$')
	[ -n "$installed" ] && installed="v$installed"
fi

# Reporting the version is not enough to know the binary works. A distribution
# package built with an older Go than this module targets prints its version
# happily and then panics on the first real run ("file requires newer Go
# version"), because it typechecks the standard library of the active toolchain.
# So the check that decides is a run, not a --version.
linter_works() {
	golangci-lint run ./... >/dev/null 2>&1
	# 1 means it ran and found issues, which is a working binary.
	[ $? -le 1 ]
}

if [ "$installed" = "$pinned" ] && linter_works; then
	note "already $pinned, and it runs. Nothing to do."
else
	if [ -z "$installed" ]; then
		note "not installed."
	elif [ "$installed" != "$pinned" ]; then
		note "found $installed, want $pinned."
	else
		note "$installed is installed but fails to run on this tree (likely built with an older Go)."
	fi

	if [ "$check_only" -eq 1 ]; then
		note "would install: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$pinned"
		due=1
	else
		command -v go >/dev/null 2>&1 || fail "Go is not installed; see go.mod for the version this module targets."
		note "installing $pinned with the local Go toolchain..."
		go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$pinned" || fail "go install failed."

		gobin=$(go env GOBIN); [ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
		case ":$PATH:" in
			*":$gobin:"*) ;;
			*) note "WARNING: $gobin is not in your PATH; add it or the hook will not find the linter." ;;
		esac

		hash -r 2>/dev/null
		if linter_works; then
			note "installed and verified by running it."
		else
			fail "Installed $pinned but it still fails to run. Check that '$gobin' comes before any other golangci-lint in your PATH: $(command -v -a golangci-lint 2>/dev/null | tr '\n' ' ')"
		fi
	fi
fi

# --- the pre-push hook ----------------------------------------------------

echo
echo "pre-push hook"

# Ask git where hooks live rather than assuming .git/hooks: a contributor with
# core.hooksPath set would otherwise get a hook written where git never looks,
# which is worse than no hook because it reads as protection.
hooks_dir=$(git rev-parse --git-path hooks)
target="$hooks_dir/pre-push"
source=scripts/hooks/pre-push

if [ -f "$target" ] && cmp -s "$source" "$target"; then
	note "already installed at $target."
else
	if [ "$check_only" -eq 1 ]; then
		note "would install $source -> $target"
		due=1
	else
		mkdir -p "$hooks_dir" || fail "Could not create $hooks_dir."
		if [ -f "$target" ]; then
			cp "$target" "$target.bak" && note "existing hook backed up to $target.bak"
		fi
		install -m 755 "$source" "$target" || fail "Could not install the hook."
		note "installed at $target."
	fi
fi

# core.hooksPath does not merge directories, it replaces them, so pointing it at
# a versioned directory would also disable any hook installed by a global
# template (a secret scanner, typically). Say so rather than let it be found the
# hard way.
if [ -n "$(git config --get core.hooksPath)" ]; then
	note "note: core.hooksPath is set, so hooks in .git/hooks are ignored. The hook above went to the configured directory."
fi

echo
if [ "${due:-0}" -eq 1 ]; then
	echo "Setup incomplete. Run ./scripts/setup-dev.sh to apply."
	exit 1
fi
echo "Ready. 'git push' now runs make lint and make test first."
