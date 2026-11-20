#!/bin/sh
# Create the tags for one release of the root module and its submodules.
#
#   scripts/release-tags.sh v0.1.6
#
# Go resolves a submodule from its own tag (adapters/pocketbase/v0.1.6), and each
# submodule's go.mod requires the root (and stockoauth) at the release version,
# so all tags go on the same commit. The script refuses a dirty tree or a
# go.mod that requires another version, creates the tags locally, and prints
# the push command. It pushes nothing.
set -eu

version="${1:-}"
if ! printf '%s\n' "$version" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
	echo "usage: $0 vMAJOR.MINOR.PATCH" >&2
	exit 2
fi

cd "$(dirname "$0")/.."
if [ -n "$(git status --porcelain)" ]; then
	echo "the working tree is not clean; commit first" >&2
	exit 1
fi

submodules="auth/stockoauth adapters/pocketbase adapters/pbpostgress"
# What each submodule must require at the release version.
requires() {
	case "$1" in
	adapters/pocketbase) echo "github.com/compdani/pocketbase-mcp github.com/compdani/pocketbase-mcp/auth/stockoauth" ;;
	*) echo "github.com/compdani/pocketbase-mcp" ;;
	esac
}
status=0
for dir in $submodules; do
	for dep in $(requires "$dir"); do
		have=$(awk -v dep="$dep" '$1 == dep { print $2 } $1 == "require" && $2 == dep { print $3 }' "$dir/go.mod")
		if [ "$have" != "$version" ]; then
			echo "$dir/go.mod requires $dep ${have:-(nothing)}, not $version" >&2
			status=1
		fi
	done
	if grep -q '^replace github.com/compdani/pocketbase-mcp' "$dir/go.mod"; then
		echo "$dir/go.mod has a replace for this repo; consumers would ignore it" >&2
		status=1
	fi
done
[ "$status" -eq 0 ] || exit 1

tags="$version"
for dir in $submodules; do
	tags="$tags $dir/$version"
done
for tag in $tags; do
	if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
		echo "tag $tag already exists" >&2
		exit 1
	fi
done
for tag in $tags; do
	git tag -a "$tag" -m "$tag"
	echo "tagged $tag"
done
echo
echo "Push with: git push origin $tags"
