#!/usr/bin/env bash
# release.sh VERSION — cut a loom release (run via `make release VERSION=v0.0.3`).
#
# loom is two Go modules: the root (github.com/rhaqim/loom) and
# generator/anthropic (github.com/rhaqim/loom/generator/anthropic), which
# requires loom. Both are released from ONE commit:
#
#   1. If generator/anthropic changed since its last tag, its go.mod is pointed
#      at loom VERSION and that change is committed. (A go.mod may name a
#      version before the tag exists; it only has to exist when downloaded.)
#   2. That commit is tagged VERSION and, when the generator changed,
#      generator/anthropic/VERSION.
#
# An unchanged generator is not retagged: its require is a minimum, so users
# get the newer loom anyway. Nothing is pushed; the push command is printed.
#
# Set RELEASE_BRANCH to release from a branch other than main.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-}"
BRANCH="${RELEASE_BRANCH:-main}"
GEN=generator/anthropic

die() { echo "release: $*" >&2; exit 1; }

[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
  die "VERSION must look like v1.2.3 (got '${VERSION}'); usage: make release VERSION=v1.2.3"

current="$(git rev-parse --abbrev-ref HEAD)"
[ "$current" = "$BRANCH" ] || die "on branch '$current', not '$BRANCH' (set RELEASE_BRANCH to override)"
[ -z "$(git status --porcelain)" ] || die "working tree is not clean; commit or stash first"

for t in "$VERSION" "$GEN/$VERSION"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then die "tag $t already exists"; fi
done

# Release the generator only if it changed since its last release (or was
# never released).
last_gen_tag="$(git tag --list "$GEN/v*" --sort=-v:refname | head -1)"
if [ -z "$last_gen_tag" ] || [ -n "$(git diff --name-only "$last_gen_tag" HEAD -- "$GEN")" ]; then
  release_gen=1
else
  release_gen=0
fi

echo "release: running make check"
make check >/dev/null || die "make check failed; nothing was changed"

if [ "$release_gen" = 1 ]; then
  (cd "$GEN" && go mod edit -require="github.com/rhaqim/loom@$VERSION")
  if [ -n "$(git status --porcelain -- "$GEN/go.mod")" ]; then
    git add "$GEN/go.mod"
    git commit -q -m "Release $VERSION: $GEN requires loom $VERSION"
  fi
fi

git tag -a "$VERSION" -m "loom $VERSION"
tags="$VERSION"
if [ "$release_gen" = 1 ]; then
  git tag -a "$GEN/$VERSION" -m "loom $GEN $VERSION"
  tags="$tags $GEN/$VERSION"
else
  echo "release: $GEN unchanged since $last_gen_tag; not retagged"
fi

echo "release: tagged $tags on $(git rev-parse --short HEAD)"
echo "release: review with 'git show $VERSION', then publish with:"
echo "  git push origin $BRANCH $tags"
