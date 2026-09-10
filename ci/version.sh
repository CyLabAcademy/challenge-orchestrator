#!/usr/bin/env sh
# The one definition of cork's version string.
#
# Every build path stamps the same value into both binaries, and they disagreed
# before this existed: the CI workflow used `--tags --always`, the release
# workflow `--tags`, and the e2e image `--tags --always --dirty`. Worse, CI
# stamped only the daemon's variable, so a `cmgrd-cli` built anywhere but a
# release reported its version as "unknown".
#
# The flags, and why each one is here:
#
#   --tags    describe against any tag, not only annotated ones
#   --always  fall back to a bare commit id rather than failing. A checkout with
#             no tags in it -- which is what actions/checkout gives you unless
#             it is asked for depth 0 -- would otherwise abort the build
#   --dirty   mark a build made from a modified tree, so a binary built from an
#             edit someone forgot to commit does not claim to be the tag
#
# The canonical form is therefore one of:
#
#   v0.1.3                     an exact tag
#   v0.1.3-74-g59410e8         74 commits past v0.1.3
#   v0.1.3-74-g59410e8-dirty   ...from a modified tree
#   59410e8                    no tag reachable (a shallow checkout)
#   unknown                    not a git tree at all
#
# Anything comparing these has to reckon with the fact that git's `-74-g<sha>`
# suffix sorts BEFORE the tag under semver pre-release rules while meaning the
# opposite. Compare the tag portion, and treat a suffix as "newer than that tag".
#
# With --ldflags it prints the whole -X stamp instead of the bare string. The
# package path appears twice per build path and there are three of them, so
# every build carried six copies of a name that is about to change: the daemon
# reads cmgr.version, the CLI its own main.version, and the CLI deliberately
# imports no cmgr package, so the two variables cannot be collapsed into one.
# Naming them here means a package rename edits this file, plus
# e2e/cork.Dockerfile, which builds inside a container with no git and so
# constructs its own stamp from the CORK_VERSION build arg. Two, not six.
set -eu

# Anchored to this script's repository, not the caller's directory. Run from
# anywhere else it would silently report `unknown` with a zero exit, and every
# binary built by that caller would claim to be an unknown version.
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

version=$(git describe --tags --always --dirty 2>/dev/null || echo unknown)

case "${1:-}" in
  --ldflags)
    [ $# -eq 1 ] || { echo "usage: version.sh [--ldflags]" >&2; exit 2; }
    printf -- '-X github.com/CyLabAcademy/challenge-orchestrator/cmgr.version=%s -X main.version=%s' "$version" "$version"
    ;;
  '')
    printf '%s\n' "$version"
    ;;
  *)
    echo "usage: version.sh [--ldflags]" >&2
    exit 2
    ;;
esac
