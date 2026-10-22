#!/bin/sh
# Builds the extension, lays it out as rhun installs it, and runs the conformance checker
# (rhun-editor/extension-protocol, conformance/ext) against it. usage: tests/check.sh
# RHUN_EXT_CHECK=path runs a checker you built instead of `go run …@latest`; RHUN_EXT_SDK=DIR builds
# against an SDK checkout (rhun-editor/extension-protocol) and runs its checker.
set -e
cd "$(dirname "$0")/.."
name=$(sed -n 's/^name *= *//p' extension.ini | head -1)
owner=$(sed -n 's/^owner *= *//p' extension.ini | head -1)
case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) target=linux-x86_64 ;;
    Darwin-arm64) target=macos-arm64 ;;
    *) echo "no target for $(uname -s)-$(uname -m)"; exit 77 ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
if [ -n "$RHUN_EXT_SDK" ]; then
    # an SDK checkout instead of the released module, and its checker
    printf 'go 1.25\n\nuse (\n\t%s\n\t%s\n)\n' "$PWD" "$RHUN_EXT_SDK" > "$work/go.work"
    export GOWORK="$work/go.work"
    if [ -z "$RHUN_EXT_CHECK" ]; then
        (cd "$RHUN_EXT_SDK" && go build -o "$work/check" ./conformance/ext)
        RHUN_EXT_CHECK=$work/check
    fi
fi
dir=$work/$owner/$name
mkdir -p "$dir/bin"
sh tests/layout.sh "$dir"  # what the release ships: the manifest, the icon, themes/, icon-themes/, ...
CGO_ENABLED=0 go build -o "$dir/bin/$name-$target" .
if [ -n "$RHUN_EXT_CHECK" ]; then
    "$RHUN_EXT_CHECK" "$dir"
else
    go run github.com/rhun-editor/extension-protocol/conformance/ext@latest "$dir"
fi
