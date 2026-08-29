#!/bin/sh
set -eu

# Let the selected go binary locate its matching standard library.
unset GOROOT

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"

version=$(sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' little_bigtable.go)
if [ -z "$version" ]; then
    echo "ERROR: Could not read the version from little_bigtable.go" >&2
    exit 1
fi

echo "... running tests"
"$ROOT/test.sh"

dist_dir="$ROOT/dist"
build_dir=$(mktemp -d "$ROOT/.dist.XXXXXX")
cleanup() {
    if [ -n "$build_dir" ]; then
        rm -rf "$build_dir"
    fi
}
trap cleanup 0
trap 'exit 1' 1 2 15

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    goos=${target%/*}
    goarch=${target##*/}
    binary="little_bigtable-${goos}-${goarch}"
    archive="little_bigtable-${version}-${goos}-${goarch}.tar.gz"

    echo "... building $binary"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
        go build -buildvcs=false -trimpath -ldflags="-s -w" \
        -o "$build_dir/$binary" .
    COPYFILE_DISABLE=1 tar -C "$build_dir" -czf "$build_dir/$archive" "$binary"
    rm "$build_dir/$binary"
done

rm -rf "$dist_dir"
mv "$build_dir" "$dist_dir"
build_dir=

echo "... archives written to $dist_dir"
