#!/bin/sh
# Checks that core-ref in each workflow resolves to the .core submodule commit.
# The marketplace builder checks out core-ref itself, so they must match.
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
	# For annotated tags the commit is on the ^{} line, which sorts last.
	commit=$(git ls-remote "$url" "refs/tags/$ref" "refs/tags/$ref^{}" "refs/heads/$ref" | sort -k2 | tail -n 1 | cut -f1)
	if [ -z "$commit" ]; then
		echo "core-ref $ref: no such tag or branch in $url" >&2
		status=1
	elif [ "$commit" != "$pinned" ]; then
		echo "core-ref $ref is $commit but .core is $pinned" >&2
		status=1
	else
		echo "core-ref $ref matches .core ($pinned)"
	fi
done

exit "$status"
