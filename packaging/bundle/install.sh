#!/bin/sh
set -eu

umask 077

BUNDLE_OS='@BUNDLE_OS@'
BUNDLE_ARCH='@BUNDLE_ARCH@'
BUNDLE_VERSION='@BUNDLE_VERSION@'
HERDR_VERSION='@HERDR_VERSION@'
HERDR_PROTOCOL='@HERDR_PROTOCOL@'
HERDR_DOWNLOAD_URL='@HERDR_DOWNLOAD_URL@'
HERDR_SHA256='@HERDR_SHA256@'

script_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
temporary_file=''
download_dir=''

cleanup() {
	if [ -n "$temporary_file" ]; then
		rm -f "$temporary_file"
		temporary_file=''
	fi
	if [ -n "$download_dir" ]; then
		rm -rf "$download_dir"
		download_dir=''
	fi
}
trap cleanup 0
trap 'exit 1' 1 2 15

fail() {
	printf '安装失败：%s\n' "$1" >&2
	exit 1
}

normalize_os() {
	case "$1" in
		Darwin | darwin) printf '%s' darwin ;;
		Linux | linux) printf '%s' linux ;;
		*) printf '%s' unsupported ;;
	esac
}

normalize_arch() {
	case "$1" in
		x86_64 | amd64) printf '%s' amd64 ;;
		arm64 | aarch64) printf '%s' arm64 ;;
		*) printf '%s' unsupported ;;
	esac
}

canonicalize_binary() {
	canonical_input=$1
	case "$canonical_input" in
		/*) ;;
		*) canonical_input=$(pwd)/$canonical_input ;;
	esac
	canonical_directory=$(dirname "$canonical_input")
	canonical_name=$(basename "$canonical_input")
	canonical_directory=$(CDPATH= cd -P "$canonical_directory" 2>/dev/null && pwd) || return 1
	printf '%s/%s' "$canonical_directory" "$canonical_name"
}

inspect_herdr() {
	inspect_path=$1
	inspected_version=unknown
	inspected_protocol=unknown
	inspect_version_output=$("$inspect_path" --version 2>/dev/null || true)
	inspect_version=$(printf '%s\n' "$inspect_version_output" | awk 'NR == 1 && $1 == "herdr" { print $2; exit }')
	if [ -n "$inspect_version" ]; then
		inspected_version=$inspect_version
	fi
	inspect_schema_output=$("$inspect_path" api schema --json 2>/dev/null || true)
	inspect_protocol=$(printf '%s\n' "$inspect_schema_output" | sed -n 's/.*"protocol"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | sed -n '1p')
	if [ -n "$inspect_protocol" ]; then
		inspected_protocol=$inspect_protocol
	fi
	[ "$inspected_version" = "$HERDR_VERSION" ] && [ "$inspected_protocol" = "$HERDR_PROTOCOL" ]
}

validate_binary_format() {
	format_path=$1
	if ! command -v file >/dev/null 2>&1; then
		fail "缺少 file 命令，无法校验下载的 Herdr 平台。"
	fi
	format_output=$(file "$format_path" 2>/dev/null) || fail "无法识别下载的 Herdr 文件格式。"
	case "$BUNDLE_OS-$BUNDLE_ARCH" in
		linux-amd64)
			printf '%s\n' "$format_output" | grep -Eq 'ELF 64-bit.*(x86-64|x86_64)' || fail "下载的 Herdr 不是 Linux amd64 ELF。"
			;;
		linux-arm64)
			printf '%s\n' "$format_output" | grep -Eq 'ELF 64-bit.*(ARM aarch64|aarch64)' || fail "下载的 Herdr 不是 Linux arm64 ELF。"
			;;
		darwin-amd64)
			printf '%s\n' "$format_output" | grep -Eq 'Mach-O 64-bit.*(x86_64|x86-64)' || fail "下载的 Herdr 不是 macOS amd64 Mach-O。"
			;;
		darwin-arm64)
			printf '%s\n' "$format_output" | grep -Eq 'Mach-O 64-bit.*arm64' || fail "下载的 Herdr 不是 macOS arm64 Mach-O。"
			;;
	esac
}

sha256_file() {
	sha_path=$1
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$sha_path" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$sha_path" | awk '{print $1}'
	else
		return 1
	fi
}

download_herdr() {
	if ! command -v curl >/dev/null 2>&1; then
		fail "缺少 curl，无法下载 Herdr ${HERDR_VERSION}；请安装 curl 后重试。"
	fi
	download_dir=$(mktemp -d "${TMPDIR:-/tmp}/herdr-pal-install.XXXXXX") || fail "无法创建下载临时目录。"
	download_path=$download_dir/herdr
	printf '正在下载兼容 Herdr %s...\n' "$HERDR_VERSION"
	if ! curl -fL --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 10 --max-time 120 "$HERDR_DOWNLOAD_URL" -o "$download_path"; then
		fail "下载 Herdr ${HERDR_VERSION} 失败：${HERDR_DOWNLOAD_URL}"
	fi
	download_sha256=$(sha256_file "$download_path") || fail "缺少 sha256sum 或 shasum，无法校验 Herdr。"
	if [ "$download_sha256" != "$HERDR_SHA256" ]; then
		fail "Herdr SHA-256 校验失败，下载内容可能损坏或被替换。"
	fi
	validate_binary_format "$download_path"
	chmod 0755 "$download_path" || fail "无法设置下载的 Herdr 执行权限。"
	if ! inspect_herdr "$download_path"; then
		fail "下载的 Herdr 不兼容：版本 ${inspected_version}，协议 ${inspected_protocol}；要求版本 ${HERDR_VERSION}、协议 ${HERDR_PROTOCOL}。"
	fi
	herdr_install_source=$download_path
}

current_os=$(normalize_os "$(uname -s)")
current_arch=$(normalize_arch "$(uname -m)")
if [ "$current_os" != "$BUNDLE_OS" ] || [ "$current_arch" != "$BUNDLE_ARCH" ]; then
	fail "安装包平台不匹配：当前为 ${current_os}/${current_arch}，安装包为 ${BUNDLE_OS}/${BUNDLE_ARCH}。"
fi

if [ ! -f "$script_dir/herdr-pal" ] || [ ! -x "$script_dir/herdr-pal" ]; then
	fail "安装包缺少可执行文件 herdr-pal。"
fi

default_install_dir=$HOME/.local/bin
printf '安装目录 [%s]: ' "$default_install_dir"
IFS= read -r install_dir || install_dir=''
case "$install_dir" in
	'') install_dir=$default_install_dir ;;
	'~') install_dir=$HOME ;;
	'~/'*) install_dir=$HOME/${install_dir#\~/} ;;
esac
case "$install_dir" in
	/*) ;;
	*) fail "安装目录必须是绝对路径或以 ~/ 开头。" ;;
esac
if ! mkdir -p "$install_dir"; then
	fail "无法创建安装目录 $install_dir。"
fi
if [ ! -d "$install_dir" ] || [ ! -w "$install_dir" ]; then
	fail "安装目录不可写：$install_dir。"
fi
install_dir=$(CDPATH= cd -P "$install_dir" && pwd)

herdr_target=$install_dir/herdr
herdr_path=''
herdr_install_source=''
need_install_herdr=0
unsupported_herdr=0
checked_herdr=''

if [ -e "$herdr_target" ] || [ -L "$herdr_target" ]; then
	target_candidate=$(canonicalize_binary "$herdr_target") || target_candidate=$herdr_target
	checked_herdr=$target_candidate
	if inspect_herdr "$target_candidate"; then
		herdr_path=$target_candidate
		printf '复用兼容 Herdr %s：%s\n' "$HERDR_VERSION" "$herdr_path"
	else
		unsupported_herdr=1
		printf '检测到不兼容 Herdr：%s（版本 %s，协议 %s）\n' "$target_candidate" "$inspected_version" "$inspected_protocol"
	fi
fi

if [ -z "$herdr_path" ]; then
	path_candidate=$(command -v herdr 2>/dev/null || true)
	if [ -n "$path_candidate" ]; then
		path_candidate=$(canonicalize_binary "$path_candidate") || true
	fi
	if [ -n "$path_candidate" ] && [ "$path_candidate" != "$checked_herdr" ]; then
		if inspect_herdr "$path_candidate"; then
			herdr_path=$path_candidate
			printf '复用兼容 Herdr %s：%s\n' "$HERDR_VERSION" "$herdr_path"
		else
			unsupported_herdr=1
			printf '检测到不兼容 Herdr：%s（版本 %s，协议 %s）\n' "$path_candidate" "$inspected_version" "$inspected_protocol"
		fi
	fi
fi

if [ -z "$herdr_path" ]; then
	if [ "$unsupported_herdr" -eq 1 ]; then
		printf '当前 Herdr 不在兼容名单中。安装兼容 Herdr %s 到 %s？[Y/n]: ' "$HERDR_VERSION" "$herdr_target"
		IFS= read -r update_choice || update_choice=''
		case "$update_choice" in
			'' | y | Y | yes | YES) ;;
			*) fail "用户取消安装兼容 Herdr，未修改 Pal 或 Herdr 配置。" ;;
		esac
	else
		printf '未检测到 Herdr，将安装兼容版本 %s 到 %s。\n' "$HERDR_VERSION" "$herdr_target"
	fi
	need_install_herdr=1
	download_herdr
	herdr_path=$herdr_target
fi

printf 'HPRP Server URL（wss://...）: '
IFS= read -r relay_url || relay_url=''
if [ -z "$relay_url" ]; then
	fail "Server URL 不能为空。"
fi

printf '机器 Key: '
IFS= read -r relay_key || relay_key=''
if [ -z "$relay_key" ]; then
	fail "机器 Key 不能为空。"
fi

backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)-$$
installed_backup=''

install_binary() {
	install_source=$1
	install_target=$2
	installed_backup=''
	if [ -e "$install_target" ] || [ -L "$install_target" ]; then
		if [ -d "$install_target" ]; then
			return 1
		fi
		installed_backup=$install_target.bak-$backup_stamp
		if ! mv "$install_target" "$installed_backup"; then
			return 1
		fi
	fi
	temporary_file=$(mktemp "$install_dir/.herdr-pal-install.XXXXXX") || return 1
	if ! cp "$install_source" "$temporary_file" || ! chmod 0755 "$temporary_file" || ! mv "$temporary_file" "$install_target"; then
		rm -f "$temporary_file"
		temporary_file=''
		if [ -n "$installed_backup" ]; then
			mv "$installed_backup" "$install_target" 2>/dev/null || true
		fi
		return 1
	fi
	temporary_file=''
	return 0
}

restore_binary() {
	restore_target=$1
	restore_backup=$2
	rm -f "$restore_target"
	if [ -n "$restore_backup" ]; then
		mv "$restore_backup" "$restore_target"
	fi
}

herdr_changed=0
herdr_backup=''
if [ "$need_install_herdr" -eq 1 ]; then
	if ! install_binary "$herdr_install_source" "$herdr_target"; then
		fail "无法安装 Herdr 到 $herdr_target。"
	fi
	herdr_backup=$installed_backup
	herdr_changed=1
fi

pal_target=$install_dir/herdr-pal
if ! install_binary "$script_dir/herdr-pal" "$pal_target"; then
	if [ "$herdr_changed" -eq 1 ]; then
		restore_binary "$herdr_target" "$herdr_backup"
	fi
	fail "无法安装 Herdr Pal 到 $pal_target，已恢复本次替换的 Herdr。"
fi
pal_backup=$installed_backup

client_config=$HOME/.config/herdr-pal/config.json
if [ -n "${XDG_CONFIG_HOME:-}" ]; then
	herdr_config=$XDG_CONFIG_HOME/herdr/config.toml
else
	herdr_config=$HOME/.config/herdr/config.toml
fi
if ! printf '%s\n' "$relay_key" | "$pal_target" setup \
	--url "$relay_url" \
	--config "$client_config" \
	--herdr-config "$herdr_config" \
	--herdr-bin "$herdr_path"; then
	relay_key=''
	restore_binary "$pal_target" "$pal_backup"
	if [ "$herdr_changed" -eq 1 ]; then
		restore_binary "$herdr_target" "$herdr_backup"
	fi
	fail "配置写入失败，已恢复本次替换的二进制；原配置未被静默覆盖。"
fi
relay_key=''

if ! HERDR_CONFIG_PATH=$herdr_config "$herdr_path" config check; then
	fail "Herdr 最终配置校验失败，请使用生成的配置备份恢复。"
fi

case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*)
		printf '\n安装目录当前不在 PATH 中，可加入 shell 配置：\n'
		printf 'export PATH="%s:$PATH"\n' "$install_dir"
		;;
esac

server_status=$(HERDR_CONFIG_PATH=$herdr_config "$herdr_path" status server --json 2>/dev/null || true)
if printf '%s\n' "$server_status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
	printf '\n检测到 Herdr 正在运行，立即执行 live-handoff 加载 Startup 插件？[Y/n]: '
	IFS= read -r handoff_choice || handoff_choice=''
	case "$handoff_choice" in
		'' | y | Y | yes | YES)
			if ! HERDR_CONFIG_PATH=$herdr_config "$herdr_path" server live-handoff --import-exe "$herdr_path"; then
				printf '%s\n' "live-handoff 未成功，现有 Herdr 保持运行；请稍后手工重启 Herdr。" >&2
			fi
			;;
		*) printf '%s\n' "已跳过 live-handoff，请手工重启 Herdr 以加载 Startup 插件。" ;;
	esac
fi

printf '\nHerdr Pal Bundle %s 安装完成。\n' "$BUNDLE_VERSION"
printf 'Herdr: %s\nHerdr Pal: %s\n' "$herdr_path" "$pal_target"
printf '回到企业微信执行 /ls 验证连接。\n'

# 保留成功安装产生的二进制备份，便于用户回滚。
: "$herdr_backup" "$pal_backup"
