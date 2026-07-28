#!/bin/sh
set -eu

export GOTOOLCHAIN=go1.26.5+auto

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
build_binary dist/hp-cli ./cmd/hp-cli
build_binary dist/herdr-pal-darwin-amd64 ./cmd/herdr-pal darwin amd64
build_binary dist/herdr-pal-server-darwin-amd64 ./cmd/herdr-pal-server darwin amd64
build_binary dist/hp-cli-darwin-amd64 ./cmd/hp-cli darwin amd64
build_binary dist/herdr-pal-darwin-arm64 ./cmd/herdr-pal darwin arm64
build_binary dist/herdr-pal-server-darwin-arm64 ./cmd/herdr-pal-server darwin arm64
build_binary dist/hp-cli-darwin-arm64 ./cmd/hp-cli darwin arm64
build_binary dist/herdr-pal-linux-amd64 ./cmd/herdr-pal linux amd64
build_binary dist/herdr-pal-server-linux-amd64 ./cmd/herdr-pal-server linux amd64
build_binary dist/hp-cli-linux-amd64 ./cmd/hp-cli linux amd64
build_binary dist/herdr-pal-linux-arm64 ./cmd/herdr-pal linux arm64
build_binary dist/herdr-pal-server-linux-arm64 ./cmd/herdr-pal-server linux arm64
build_binary dist/hp-cli-linux-arm64 ./cmd/hp-cli linux arm64
build_binary dist/herdr-pal-windows-amd64.exe ./cmd/herdr-pal windows amd64

write_checksums() {
	(
		cd dist
		set -- \
			herdr-pal-darwin-amd64 \
			herdr-pal-server-darwin-amd64 \
			hp-cli-darwin-amd64 \
			herdr-pal-darwin-arm64 \
			herdr-pal-server-darwin-arm64 \
			hp-cli-darwin-arm64 \
			herdr-pal-linux-amd64 \
			herdr-pal-server-linux-amd64 \
			hp-cli-linux-amd64 \
			herdr-pal-linux-arm64 \
			herdr-pal-server-linux-arm64 \
			hp-cli-linux-arm64 \
			herdr-pal-windows-amd64.exe
		if command -v sha256sum >/dev/null 2>&1; then
			sha256sum "$@" > SHA256SUMS
		elif command -v shasum >/dev/null 2>&1; then
			shasum -a 256 "$@" > SHA256SUMS
		else
			printf '%s\n' "缺少 sha256sum 或 shasum，无法生成发布校验和。" >&2
			exit 1
		fi
	)
}

write_checksums
