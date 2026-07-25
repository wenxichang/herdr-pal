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
LD_FLAGS="-X github.com/wenxichang/herdr-pal/internal/version.Version=${VERSION} -X github.com/wenxichang/herdr-pal/internal/version.Commit=${COMMIT} -X github.com/wenxichang/herdr-pal/internal/version.BuiltAt=${BUILT_AT}"

build_binary() {
	if [ "$#" -eq 4 ]; then
		GOOS="$3" GOARCH="$4" CGO_ENABLED=0 go build -trimpath \
			-ldflags "$LD_FLAGS" \
			-o "$1" \
			"$2"
		return
	fi
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "$LD_FLAGS" \
		-o "$1" \
		"$2"
}

build_binary dist/herdr-pal ./cmd/herdr-pal
build_binary dist/herdr-pal-server ./cmd/herdr-pal-server
build_binary dist/herdr-pal-darwin-amd64 ./cmd/herdr-pal darwin amd64
build_binary dist/herdr-pal-server-darwin-amd64 ./cmd/herdr-pal-server darwin amd64
build_binary dist/herdr-pal-darwin-arm64 ./cmd/herdr-pal darwin arm64
build_binary dist/herdr-pal-server-darwin-arm64 ./cmd/herdr-pal-server darwin arm64
build_binary dist/herdr-pal-linux-amd64 ./cmd/herdr-pal linux amd64
build_binary dist/herdr-pal-server-linux-amd64 ./cmd/herdr-pal-server linux amd64
build_binary dist/herdr-pal-linux-arm64 ./cmd/herdr-pal linux arm64
build_binary dist/herdr-pal-server-linux-arm64 ./cmd/herdr-pal-server linux arm64
