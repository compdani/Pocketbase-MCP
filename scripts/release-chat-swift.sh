#!/bin/sh
# Prepare a release of the Swift chat package (frontend/swift) for its mirror
# repository, so SwiftPM consumers can depend on a tagged version:
#
#   scripts/release-chat-swift.sh v0.1.0
#
# SwiftPM requires Package.swift at the repository root, so the Swift package's
# history is split out (git subtree split) into a tree whose root is the package.
# The script checks that frontend/swift/VERSION has the release version and the
# tree is clean, validates Package.swift, runs the Swift tests, creates the local
# monorepo tag chat-swift/vX.Y.Z on the split commit, and prints the push command
# that publishes vX.Y.Z on the standalone mirror repository. It pushes nothing.
#
# MIRROR overrides the mirror repository URL.
set -eu

version="${1:-}"
if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	echo "usage: $0 vMAJOR.MINOR.PATCH" >&2
	exit 2
fi
mirror="${MIRROR:-git@github.com:compdani/pocketbase-mcp-chat-swift.git}"

cd "$(dirname "$0")/.."
if [ -n "$(git status --porcelain)" ]; then
	echo "the working tree is not clean; commit first" >&2
	exit 1
fi

have=$(tr -d ' \t\r\n' < frontend/swift/VERSION 2>/dev/null || true)
if [ "v$have" != "$version" ]; then
	echo "frontend/swift/VERSION has version ${have:-(none)}, not ${version#v}" >&2
	exit 1
fi

tag="chat-swift/$version"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "tag $tag already exists" >&2
	exit 1
fi

swift package --package-path frontend/swift describe >/dev/null || {
	echo "frontend/swift/Package.swift failed validation; fix it first" >&2
	exit 1
}

swift test --package-path frontend/swift >/dev/null || {
	echo "the Swift tests fail; fix them first" >&2
	exit 1
}

split=$(git subtree split --prefix=frontend/swift HEAD)
git tag -a "$tag" -m "chat-swift $version" "$split"
echo "tagged $tag at $split (the Swift package's own history)"
echo
echo "Push with: git push $mirror $split:refs/heads/main $tag^{}:refs/tags/$version"
