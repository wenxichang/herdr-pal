#!/bin/sh
set -eu

usage() {
	printf '%s\n' "用法: ./packaging/build-bundle.sh --target <linux-amd64|linux-arm64|darwin-amd64|darwin-arm64> --version <版本> [--herdr-source <目录> | --herdr-binary <文件>] [--herdr-commit <提交>]" >&2
}

fail() {
	printf '打包失败：%s\n' "$1" >&2
	exit 1
}

target=''
version=''
herdr_source=''
herdr_binary=''
herdr_commit=''

while [ "$#" -gt 0 ]; do
	case "$1" in
		--target | --version | --herdr-source | --herdr-binary | --herdr-commit)
			option=$1
			shift
			if [ "$#" -eq 0 ]; then
				usage
				exit 2
			fi
			case "$option" in
				--target) target=$1 ;;
				--version) version=$1 ;;
				--herdr-source) herdr_source=$1 ;;
				--herdr-binary) herdr_binary=$1 ;;
				--herdr-commit) herdr_commit=$1 ;;
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
if [ -n "$herdr_source" ] && [ -n "$herdr_binary" ]; then
	fail "--herdr-source 与 --herdr-binary 不能同时使用。"
fi

case "$target" in
	linux-amd64)
		rust_target=x86_64-unknown-linux-musl
		pal_name=herdr-pal-linux-amd64
		;;
	linux-arm64)
		rust_target=aarch64-unknown-linux-musl
		pal_name=herdr-pal-linux-arm64
		;;
	darwin-amd64)
		rust_target=x86_64-apple-darwin
		pal_name=herdr-pal-darwin-amd64
		;;
	darwin-arm64)
		rust_target=aarch64-apple-darwin
		pal_name=herdr-pal-darwin-arm64
		;;
	*) fail "不支持目标平台 $target。" ;;
esac

repo_root=$(CDPATH= cd -P "$(dirname "$0")/.." && pwd)
pal_binary=$repo_root/dist/$pal_name
if [ ! -f "$pal_binary" ]; then
	fail "缺少 Herdr Pal 目标文件 $pal_binary，请先运行 ./build.sh。"
fi

if [ -z "$herdr_binary" ]; then
	if [ -z "$herdr_source" ]; then
		herdr_source=${HERDR_SOURCE_DIR:-$HOME/Code/herdr}
	fi
	if [ ! -f "$herdr_source/Cargo.toml" ]; then
		fail "Herdr 源码目录无效：$herdr_source。"
	fi
	if [ -n "$(git -C "$herdr_source" status --porcelain 2>/dev/null || printf '%s' invalid)" ]; then
		fail "Herdr 源码工作区必须是干净的 Git 仓库。"
	fi
	printf '构建 Herdr：%s (%s)\n' "$herdr_source" "$rust_target"
	(
		cd "$herdr_source"
		cargo build --release --locked --target "$rust_target"
	)
	herdr_binary=$herdr_source/target/$rust_target/release/herdr
	if [ -z "$herdr_commit" ]; then
		herdr_commit=$(git -C "$herdr_source" rev-parse --short HEAD)
	fi
fi
if [ ! -f "$herdr_binary" ]; then
	fail "Herdr 二进制不存在：$herdr_binary。"
fi
if [ -z "$herdr_commit" ]; then
	herdr_commit=unknown
fi
case "$herdr_commit" in
	*[!A-Za-z0-9._+-]*) fail "Herdr 提交标识包含不安全字符。" ;;
esac

validate_binary() {
	binary_path=$1
	binary_label=$2
	file_output=$(file "$binary_path" 2>/dev/null) || fail "无法识别 $binary_label 文件格式。"
	case "$target" in
		linux-amd64)
			printf '%s\n' "$file_output" | grep -Eq 'ELF 64-bit.*(x86-64|x86_64)' || fail "$binary_label 不是 Linux amd64 ELF。"
			printf '%s\n' "$file_output" | grep -Eq '(statically linked|static-pie linked)' || fail "$binary_label 不是静态链接 Linux 文件。"
			;;
		linux-arm64)
			printf '%s\n' "$file_output" | grep -Eq 'ELF 64-bit.*(ARM aarch64|aarch64)' || fail "$binary_label 不是 Linux arm64 ELF。"
			printf '%s\n' "$file_output" | grep -Eq '(statically linked|static-pie linked)' || fail "$binary_label 不是静态链接 Linux 文件。"
			;;
		darwin-amd64)
			printf '%s\n' "$file_output" | grep -Eq 'Mach-O 64-bit.*(x86_64|x86-64)' || fail "$binary_label 不是 macOS amd64 Mach-O。"
			;;
		darwin-arm64)
			printf '%s\n' "$file_output" | grep -Eq 'Mach-O 64-bit.*arm64' || fail "$binary_label 不是 macOS arm64 Mach-O。"
			;;
	esac
}

validate_binary "$herdr_binary" Herdr
validate_binary "$pal_binary" "Herdr Pal"

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/herdr-bundle.XXXXXX")
cleanup() {
	rm -rf "$temporary_root"
}
trap cleanup 0
trap 'exit 1' 1 2 15

bundle_name=herdr-bundle-$version-$target
bundle_root=$temporary_root/$bundle_name
mkdir -p "$bundle_root"
cp "$herdr_binary" "$bundle_root/herdr"
cp "$pal_binary" "$bundle_root/herdr-pal"
chmod 0755 "$bundle_root/herdr" "$bundle_root/herdr-pal"

sed \
	-e "s|@BUNDLE_OS@|${target%-*}|g" \
	-e "s|@BUNDLE_ARCH@|${target#*-}|g" \
	-e "s|@BUNDLE_VERSION@|$version|g" \
	"$repo_root/packaging/bundle/install.sh" > "$bundle_root/install.sh"
chmod 0755 "$bundle_root/install.sh"

sed \
	-e "s|@BUNDLE_VERSION@|$version|g" \
	-e "s|@BUNDLE_TARGET@|$target|g" \
	-e "s|@HERDR_COMMIT@|$herdr_commit|g" \
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
