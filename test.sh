#!/bin/sh
set -eu

# Let the selected go binary locate its matching standard library.
unset GOROOT

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
cd "$ROOT"

timeout=${TEST_TIMEOUT:-180s}

echo "... running tests"
go test -buildvcs=false -timeout "$timeout" ./...

goos=$(go env GOOS)
goarch=$(go env GOARCH)
cgo_enabled=$(go env CGO_ENABLED)

case "$goos/$goarch" in
    linux/amd64|linux/arm64|linux/loong64|linux/ppc64le|linux/riscv64|linux/s390x|darwin/amd64|darwin/arm64|freebsd/amd64|netbsd/amd64|windows/amd64)
        if [ "$goos" = "darwin" ] || [ "$cgo_enabled" = "1" ]; then
            echo "... running race tests"
            go test -buildvcs=false -timeout "$timeout" -race ./...
        else
            echo "... skipping race tests: CGO is disabled"
        fi
        ;;
    *)
        echo "... skipping race tests: unsupported on $goos/$goarch"
        ;;
esac

echo "... running go vet"
go vet -composites=false ./...

echo "... checking gofmt"
unformatted=$(find . -type f -name '*.go' ! -path './vendor/*' -exec gofmt -l '{}' +)
if [ -n "$unformatted" ]; then
    printf '%s\n' "$unformatted"
    exit 1
fi
