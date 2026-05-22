#!/bin/sh
# scripts/bump.sh — bump the project version and push a new git tag.
#
# Usage:
#   scripts/bump.sh patch    # vX.Y.Z -> vX.Y.(Z+1)
#   scripts/bump.sh minor    # vX.Y.Z -> vX.(Y+1).0
#   scripts/bump.sh major    # vX.Y.Z -> v(X+1).0.0
#
# When no tag exists yet:
#   patch -> v0.0.1
#   minor -> v0.1.0
#   major -> v1.0.0
#
# The push triggers the Release workflow on GitHub.

set -eu

usage() {
    echo "usage: $(basename "$0") {patch|minor|major}" >&2
    exit 2
}

[ $# -eq 1 ] || usage
PART="$1"
case "$PART" in
    patch|minor|major) ;;
    *) usage ;;
esac

cd "$(git rev-parse --show-toplevel)"

if ! git diff-index --quiet HEAD --; then
    echo "Working tree has uncommitted changes. Commit or stash first." >&2
    exit 1
fi

BRANCH="$(git symbolic-ref --short HEAD 2>/dev/null || echo DETACHED)"
if [ "$BRANCH" != "main" ]; then
    printf "You are on '%s' (not main). Continue? [y/N] " "$BRANCH"
    read -r ans
    case "$ans" in
        y|Y|yes|YES) ;;
        *) echo "aborted."; exit 1 ;;
    esac
fi

git fetch --tags --quiet origin || true

LATEST="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -n1)"

if [ -z "$LATEST" ]; then
    MAJOR=0; MINOR=0; PATCH=0
else
    V="${LATEST#v}"
    MAJOR="${V%%.*}"
    REST="${V#*.}"
    MINOR="${REST%%.*}"
    PATCH="${REST#*.}"
    # Reject anything that isn't a clean integer triple (rc/beta/etc.).
    case "$MAJOR$MINOR$PATCH" in
        *[!0-9]*)
            echo "Latest tag $LATEST is not a plain semver triple, refusing to bump." >&2
            exit 1
            ;;
    esac
fi

case "$PART" in
    patch) PATCH=$((PATCH + 1)) ;;
    minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
    major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
esac

NEXT="v${MAJOR}.${MINOR}.${PATCH}"

if [ -n "$LATEST" ]; then
    echo "Latest tag: $LATEST"
fi
echo "New tag:    $NEXT"

if git rev-parse -q --verify "refs/tags/$NEXT" >/dev/null; then
    echo "Tag $NEXT already exists locally." >&2
    exit 1
fi
if git ls-remote --tags --exit-code origin "refs/tags/$NEXT" >/dev/null 2>&1; then
    echo "Tag $NEXT already exists on origin." >&2
    exit 1
fi

printf "Tag and push to origin? [y/N] "
read -r ans
case "$ans" in
    y|Y|yes|YES) ;;
    *) echo "aborted."; exit 1 ;;
esac

git tag -a "$NEXT" -m "release $NEXT"
git push origin "$NEXT"

echo
echo "Pushed $NEXT. Release workflow:"
echo "  https://github.com/nbardavid/share-terminal/actions"
