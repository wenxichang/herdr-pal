#!/bin/sh
set -eu

cd "$(dirname "$0")"

if [ -n "$(gofmt -l .)" ]; then
	printf '%s\n' "请先运行 gofmt。" >&2
	exit 1
fi

go vet ./...
go test -count=1 ./...

if [ "$(go env CGO_ENABLED)" = "1" ]; then
	go test -race ./...
fi
