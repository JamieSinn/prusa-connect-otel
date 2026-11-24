#!/bin/sh
# Fails unless the .core submodule is the commit that core-ref names in every
# workflow handing it to the marketplace's builder. The builder clones core-ref
# into .core/ itself, so the two must be one commit.
set -eu

url=$(git config -f .gitmodules submodule..core.url)
pinned=$(git rev-parse HEAD:.core)

refs=$(grep -ho 'core-ref:[[:space:]]*[^[:space:]#]*' .github/workflows/*.yml | sed 's/core-ref:[[:space:]]*//' | sort -u)
if [ -z "$refs" ]; then
	echo "no workflow names core-ref" >&2
	exit 1
fi

status=0
for ref in $refs; do
	# An annotated tag's commit is on its ^{} line, which sorts after the tag.
	commit=$(git ls-remote "$url" "refs/tags/$ref" "refs/tags/$ref^{}" "refs/heads/$ref" | sort -k2 | tail -n 1 | cut -f1)
	if [ -z "$commit" ]; then
		echo "core-ref $ref is not a tag or branch of $url" >&2
		status=1
	elif [ "$commit" != "$pinned" ]; then
		echo "core-ref $ref is $commit, but the .core submodule is $pinned" >&2
		status=1
	else
		echo "core-ref $ref is the .core submodule, $pinned"
	fi
done

exit "$status"
