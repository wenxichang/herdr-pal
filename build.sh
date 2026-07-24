#!/bin/sh
set -eu

cd "$(dirname "$0")"

if [ -n "$(gofmt -l .)" ]; then
	printf '%s\n' "请先运行 gofmt。" >&2
	exit 1
fi

go vet ./...
go test ./...

mkdir -p dist
VERSION="${VERSION:-$(git describe --tags --always --dirty)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD)}"
BUILT_AT="${BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

CGO_ENABLED=0 go build -trimpath \
	-ldflags "-X github.com/wenxichang/herdr-pal/internal/version.Version=${VERSION} -X github.com/wenxichang/herdr-pal/internal/version.Commit=${COMMIT} -X github.com/wenxichang/herdr-pal/internal/version.BuiltAt=${BUILT_AT}" \
	-o dist/herdr-pal \
	./cmd/herdr-pal

CGO_ENABLED=0 go build -trimpath \
	-ldflags "-X github.com/wenxichang/herdr-pal/internal/version.Version=${VERSION} -X github.com/wenxichang/herdr-pal/internal/version.Commit=${COMMIT} -X github.com/wenxichang/herdr-pal/internal/version.BuiltAt=${BUILT_AT}" \
	-o dist/herdr-pal-server \
	./cmd/herdr-pal-server
