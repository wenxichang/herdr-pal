#!/bin/sh
set -eu

umask 077

BUNDLE_OS='@BUNDLE_OS@'
BUNDLE_ARCH='@BUNDLE_ARCH@'
BUNDLE_VERSION='@BUNDLE_VERSION@'

script_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
stty_state=''
temporary_file=''

cleanup() {
	if [ -n "$stty_state" ]; then
		stty "$stty_state" 2>/dev/null || true
		stty_state=''
	fi
	if [ -n "$temporary_file" ]; then
		rm -f "$temporary_file"
		temporary_file=''
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

current_os=$(normalize_os "$(uname -s)")
current_arch=$(normalize_arch "$(uname -m)")
if [ "$current_os" != "$BUNDLE_OS" ] || [ "$current_arch" != "$BUNDLE_ARCH" ]; then
	fail "安装包平台不匹配：当前为 ${current_os}/${current_arch}，安装包为 ${BUNDLE_OS}/${BUNDLE_ARCH}。"
fi

for bundled_binary in herdr herdr-pal; do
	if [ ! -f "$script_dir/$bundled_binary" ] || [ ! -x "$script_dir/$bundled_binary" ]; then
		fail "安装包缺少可执行文件 $bundled_binary。"
	fi
done

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

printf 'HPRP Server URL（wss://...）: '
IFS= read -r relay_url || relay_url=''
if [ -z "$relay_url" ]; then
	fail "Server URL 不能为空。"
fi

printf '机器 Key: '
if [ -t 0 ]; then
	stty_state=$(stty -g)
	stty -echo
	IFS= read -r relay_key || relay_key=''
	stty "$stty_state"
	stty_state=''
	printf '\n'
else
	IFS= read -r relay_key || relay_key=''
fi
if [ -z "$relay_key" ]; then
	fail "机器 Key 不能为空。"
fi

backup_stamp=$(date -u +%Y%m%dT%H%M%SZ)-$$
installed_backup=''

install_binary() {
	source_path=$1
	target_path=$2
	installed_backup=''
	if [ -e "$target_path" ] || [ -L "$target_path" ]; then
		if [ -d "$target_path" ]; then
			return 1
		fi
		installed_backup=$target_path.bak-$backup_stamp
		if ! mv "$target_path" "$installed_backup"; then
			return 1
		fi
	fi
	temporary_file=$(mktemp "$install_dir/.herdr-bundle-install.XXXXXX") || return 1
	if ! cp "$source_path" "$temporary_file" || ! chmod 0755 "$temporary_file" || ! mv "$temporary_file" "$target_path"; then
		rm -f "$temporary_file"
		temporary_file=''
		if [ -n "$installed_backup" ]; then
			mv "$installed_backup" "$target_path" 2>/dev/null || true
		fi
		return 1
	fi
	temporary_file=''
	return 0
}

restore_binary() {
	target_path=$1
	backup_path=$2
	rm -f "$target_path"
	if [ -n "$backup_path" ]; then
		mv "$backup_path" "$target_path"
	fi
}

herdr_target=$install_dir/herdr
pal_target=$install_dir/herdr-pal
if ! install_binary "$script_dir/herdr" "$herdr_target"; then
	fail "无法安装 Herdr 到 $herdr_target。"
fi
herdr_backup=$installed_backup
if ! install_binary "$script_dir/herdr-pal" "$pal_target"; then
	restore_binary "$herdr_target" "$herdr_backup"
	fail "无法安装 Herdr Pal 到 $pal_target，Herdr 已恢复。"
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
	--herdr-bin "$herdr_target"; then
	relay_key=''
	fail "配置写入失败。二进制已安装，原配置未被静默覆盖。"
fi
relay_key=''

if ! HERDR_CONFIG_PATH=$herdr_config "$herdr_target" config check; then
	fail "Herdr 最终配置校验失败，请使用生成的备份恢复配置。"
fi

case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*)
		printf '\n安装目录当前不在 PATH 中，可加入 shell 配置：\n'
		printf 'export PATH="%s:$PATH"\n' "$install_dir"
		;;
esac

server_status=$(HERDR_CONFIG_PATH=$herdr_config "$herdr_target" status server --json 2>/dev/null || true)
if printf '%s\n' "$server_status" | grep -Eq '"running"[[:space:]]*:[[:space:]]*true'; then
	printf '\n检测到 Herdr 正在运行，立即执行 live-handoff 加载新版本和 Sidecar？[Y/n]: '
	IFS= read -r handoff_choice || handoff_choice=''
	case "$handoff_choice" in
		'' | y | Y | yes | YES)
			if ! HERDR_CONFIG_PATH=$herdr_config "$herdr_target" server live-handoff --import-exe "$herdr_target"; then
				printf '%s\n' "live-handoff 未成功，现有 Herdr 保持运行；请稍后手工重启 Herdr。" >&2
			fi
			;;
		*) printf '%s\n' "已跳过 live-handoff，请手工重启 Herdr 以启动 Sidecar。" ;;
	esac
fi

printf '\nHerdr Bundle %s 安装完成。\n' "$BUNDLE_VERSION"
printf 'Herdr: %s\nHerdr Pal: %s\n' "$herdr_target" "$pal_target"
printf '回到企业微信执行 /ls 验证连接。\n'

# 保留成功安装产生的二进制备份，便于用户回滚。
: "$herdr_backup" "$pal_backup"
