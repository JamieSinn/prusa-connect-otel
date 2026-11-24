#!/bin/sh
# Builds the module and installs it into a local Prusa Connect from .core.
#
#   sh install.sh             install into the demo's data (simulated printers)
#   TARGET=app sh install.sh  install into the real app's data instead
#
# `go -C .core run ./cmd/new-module -start demo -dir ..` runs this if needed.
# Rerun it after changing module.yaml or Go code.
set -e

HERE=$(cd "$(dirname "$0")" && pwd)
CHECKOUT=$HERE/.core

NAME=$(sed -n 's/^name:[[:space:]]*//p' "$HERE/module.yaml" | head -1)
VERSION=$(sed -n 's/^version:[[:space:]]*//p' "$HERE/module.yaml" | head -1)
# The path the extension is published under; see extensionModule in faults.go.
MODULE=connect-otel.jamiesinn.ca

TARGET=${TARGET:-demo}
WORK=$CHECKOUT/bin/$NAME-$TARGET
case "$TARGET" in
demo) DATA=$WORK/demo ;;
app) DATA=$WORK ;;
*)
	echo "FAIL  TARGET must be demo or app, not $TARGET."
	exit 1
	;;
esac

CONFIG=$WORK/config.yaml
RELEASE=$HERE/ui/dist
LOG=$WORK/install.log

for tool in go pnpm; do
	if ! command -v "$tool" > /dev/null 2>&1; then
		echo "FAIL  $tool is not on PATH."
		exit 1
	fi
done

if [ ! -f "$CHECKOUT/sdk/go.mod" ]; then
	echo "FAIL  .core is not checked out; run: git submodule update --init"
	exit 1
fi

mkdir -p "$WORK" "$DATA"
: > "$LOG"

STEP=0
step() {
	STEP=$((STEP + 1))
	echo "$STEP. $*"
}

# Runs a command, logging to $LOG; prints the log tail on failure.
quietly() {
	if ! "$@" >> "$LOG" 2>&1; then
		echo "FAIL  $* failed. The end of $LOG:"
		tail -n 30 "$LOG"
		exit 1
	fi
}

step "building the bundle..."
quietly make -C "$HERE" ui-build

step "building the process..."
BINARY="${NAME}_$(go env GOOS)_$(go env GOARCH)$(go env GOEXE)"
quietly go -C "$HERE" build -o "$RELEASE/$BINARY" .
chmod +x "$RELEASE/$BINARY"

# Config for module add. Paths must match where the demo keeps its data.
if [ ! -f "$CONFIG" ]; then
	cat > "$CONFIG" <<- YAML
		core:
		  overlay: $DATA/resources.json
		  modules: $WORK/modules
		  authz:
		    database: $DATA/config.d/authz.db
	YAML
fi

step "installing $MODULE@$VERSION..."
# -approve grants the requested permissions and enables the module.
quietly go -C "$CHECKOUT" run ./cmd/prusa-connect module add "$MODULE@$VERSION" \
	-config "$CONFIG" -from "$RELEASE" -approve

echo "OK    $NAME is installed in $WORK"
