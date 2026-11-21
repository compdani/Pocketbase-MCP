#!/bin/sh
# Prepare a release of the browser client (frontend/client) for its mirror
# repository, so hosts can install it with npm from a tagged tarball:
#
#   scripts/release-chat-client.sh v0.1.9
#   npm install https://codeload.github.com/compdani/pocketbase-mcp-chat-client/tar.gz/refs/tags/v0.1.9
#
# npm can't install a subdirectory of a git repository, so the client's history
# is split out (git subtree split) into a tree whose root is the package. The
# script checks that package.json has the release version and the tree is
# clean, creates the local tag chat-client/vX.Y.Z on the split commit, and
# prints the push command. It pushes nothing.
#
# MIRROR overrides the mirror repository URL.
set -eu

version="${1:-}"
if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	echo "usage: $0 vMAJOR.MINOR.PATCH" >&2
	exit 2
fi
mirror="${MIRROR:-git@github.com:compdani/pocketbase-mcp-chat-client.git}"

cd "$(dirname "$0")/.."
if [ -n "$(git status --porcelain)" ]; then
	echo "the working tree is not clean; commit first" >&2
	exit 1
fi

have=$(sed -n 's/^  "version": "\(.*\)",$/\1/p' frontend/client/package.json)
if [ "v$have" != "$version" ]; then
	echo "frontend/client/package.json has version ${have:-(none)}, not ${version#v}" >&2
	exit 1
fi

tag="chat-client/$version"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "tag $tag already exists" >&2
	exit 1
fi

(cd frontend/client && node --test >/dev/null) || {
	echo "the client tests fail; fix them first" >&2
	exit 1
}

split=$(git subtree split --prefix=frontend/client HEAD)
git tag -a "$tag" -m "chat-client $version" "$split"
echo "tagged $tag at $split (the client's own history)"
echo
echo "Push with: git push $mirror $split:refs/heads/main $tag^{}:refs/tags/$version"
