#!/bin/sh
set -eu

usage() {
	printf '%s\n' "用法: ./packaging/build-bundle.sh --target <linux-amd64|linux-arm64|darwin-amd64|darwin-arm64> --version <版本>" >&2
}

fail() {
	printf '打包失败：%s\n' "$1" >&2
	exit 1
}

target=''
version=''

while [ "$#" -gt 0 ]; do
	case "$1" in
		--target | --version)
			option=$1
			shift
			if [ "$#" -eq 0 ]; then
				usage
				exit 2
			fi
			case "$option" in
				--target) target=$1 ;;
				--version) version=$1 ;;
			esac
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage
			exit 2
			;;
	esac
	shift
done

if [ -z "$target" ] || [ -z "$version" ]; then
	usage
	exit 2
fi
case "$version" in
	*-dirty* | *[!A-Za-z0-9._+-]*) fail "版本只能包含字母、数字、点、下划线、加号和连字符，且不能是 dirty 状态。" ;;
esac

herdr_version=0.7.5
herdr_protocol=17
case "$target" in
	linux-amd64)
		pal_name=herdr-pal-linux-amd64
		herdr_url=https://github.com/herdrdev/herdr/releases/download/v0.7.5/herdr-linux-x86_64
		herdr_sha256=3dc83288073e4c2d3c679a30e7be97bcca9141c6fd17dbbb9219142e95c59253
		;;
	linux-arm64)
		pal_name=herdr-pal-linux-arm64
		herdr_url=https://github.com/herdrdev/herdr/releases/download/v0.7.5/herdr-linux-aarch64
		herdr_sha256=32e763a1499a6b694b1d708e4f062b743be1da9f34fcfa4d212d6db6fe09a8b9
		;;
	darwin-amd64)
		pal_name=herdr-pal-darwin-amd64
		herdr_url=https://github.com/herdrdev/herdr/releases/download/v0.7.5/herdr-macos-x86_64
		herdr_sha256=3fe50c4a63dc8102306b1322178628ddb3655cd3ae56d784f094153408d69e62
		;;
	darwin-arm64)
		pal_name=herdr-pal-darwin-arm64
		herdr_url=https://github.com/herdrdev/herdr/releases/download/v0.7.5/herdr-macos-aarch64
		herdr_sha256=37350546b0012555943b92eaf962665de4e264395baeb44227b8015e8ff5b0d6
		;;
	*) fail "不支持目标平台 $target。" ;;
esac

repo_root=$(CDPATH= cd -P "$(dirname "$0")/.." && pwd)
pal_binary=$repo_root/dist/$pal_name
if [ ! -f "$pal_binary" ]; then
	fail "缺少 Herdr Pal 目标文件 $pal_binary，请先运行 ./build.sh。"
fi

file_output=$(file "$pal_binary" 2>/dev/null) || fail "无法识别 Herdr Pal 文件格式。"
case "$target" in
	linux-amd64)
		printf '%s\n' "$file_output" | grep -Eq 'ELF 64-bit.*(x86-64|x86_64)' || fail "Herdr Pal 不是 Linux amd64 ELF。"
		printf '%s\n' "$file_output" | grep -Eq '(statically linked|static-pie linked)' || fail "Herdr Pal 不是静态链接 Linux 文件。"
		;;
	linux-arm64)
		printf '%s\n' "$file_output" | grep -Eq 'ELF 64-bit.*(ARM aarch64|aarch64)' || fail "Herdr Pal 不是 Linux arm64 ELF。"
		printf '%s\n' "$file_output" | grep -Eq '(statically linked|static-pie linked)' || fail "Herdr Pal 不是静态链接 Linux 文件。"
		;;
	darwin-amd64)
		printf '%s\n' "$file_output" | grep -Eq 'Mach-O 64-bit.*(x86_64|x86-64)' || fail "Herdr Pal 不是 macOS amd64 Mach-O。"
		;;
	darwin-arm64)
		printf '%s\n' "$file_output" | grep -Eq 'Mach-O 64-bit.*arm64' || fail "Herdr Pal 不是 macOS arm64 Mach-O。"
		;;
esac

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/herdr-pal-bundle.XXXXXX")
cleanup() {
	rm -rf "$temporary_root"
}
trap cleanup 0
trap 'exit 1' 1 2 15

bundle_name=herdr-pal-bundle-$version-$target
bundle_root=$temporary_root/$bundle_name
mkdir -p "$bundle_root"
cp "$pal_binary" "$bundle_root/herdr-pal"
chmod 0755 "$bundle_root/herdr-pal"

sed \
	-e "s|@BUNDLE_OS@|${target%-*}|g" \
	-e "s|@BUNDLE_ARCH@|${target#*-}|g" \
	-e "s|@BUNDLE_VERSION@|$version|g" \
	-e "s|@HERDR_VERSION@|$herdr_version|g" \
	-e "s|@HERDR_PROTOCOL@|$herdr_protocol|g" \
	-e "s|@HERDR_DOWNLOAD_URL@|$herdr_url|g" \
	-e "s|@HERDR_SHA256@|$herdr_sha256|g" \
	"$repo_root/packaging/bundle/install.sh" > "$bundle_root/install.sh"
chmod 0755 "$bundle_root/install.sh"

sed \
	-e "s|@BUNDLE_VERSION@|$version|g" \
	-e "s|@BUNDLE_TARGET@|$target|g" \
	-e "s|@HERDR_VERSION@|$herdr_version|g" \
	"$repo_root/packaging/bundle/README.md" > "$bundle_root/README.md"
chmod 0644 "$bundle_root/README.md"

mkdir -p "$repo_root/dist"
archive_path=$repo_root/dist/$bundle_name.tar.gz
rm -f "$archive_path" "$archive_path.sha256"
tar -C "$temporary_root" -czf "$archive_path" "$bundle_name"

archive_name=$(basename "$archive_path")
if command -v sha256sum >/dev/null 2>&1; then
	(
		cd "$repo_root/dist"
		sha256sum "$archive_name" > "$archive_name.sha256"
	)
elif command -v shasum >/dev/null 2>&1; then
	(
		cd "$repo_root/dist"
		shasum -a 256 "$archive_name" > "$archive_name.sha256"
	)
else
	fail "缺少 sha256sum 或 shasum。"
fi

printf '已生成：%s\n' "$archive_path"
printf '校验和：%s\n' "$archive_path.sha256"
