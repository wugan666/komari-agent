#!/bin/sh
set -eu

# 只加载校验函数，离线验证安装器，不安装服务或下载执行文件。
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
installer="$script_dir/../install.sh"
eval "$(sed -n '/^verify_asset_checksum() {/,/^}/p' "$installer")"
log_error() { printf '%s\n' "$1" >&2; }

test_dir=$(mktemp -d "${TMPDIR:-/tmp}/komari-installer-test.XXXXXX")
test_binary="$test_dir/komari-agent-linux-amd64"
test_checksum="$test_binary.sha256"
test_name="komari-agent-linux-amd64"
fixture_hash="e09932a21e68c61339c0a8db027bc45d2bd91bed1e801f4d8054d5bedc0f38b2"
cleanup() {
    rm -f "$test_binary" "$test_checksum" "$test_dir/agent" "$test_dir/service-stopped"
    rmdir "$test_dir"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

expect_rejected() {
    if verify_asset_checksum "$test_binary" "$test_checksum" "$test_name" 2>/dev/null; then
        printf 'FAIL: accepted %s\n' "$1" >&2
        exit 1
    fi
}

printf 'owned release\n' > "$test_binary"
printf '%s  %s\n' "$fixture_hash" "$test_name" > "$test_checksum"
verify_asset_checksum "$test_binary" "$test_checksum" "$test_name"
printf '%s *%s\r\n' "$fixture_hash" "$test_name" > "$test_checksum"
verify_asset_checksum "$test_binary" "$test_checksum" "$test_name"

printf 'tampered release\n' > "$test_binary"
expect_rejected 'modified binary'
printf 'owned release\n' > "$test_binary"
printf '%s  %s.exe\n' "$fixture_hash" "$test_name" > "$test_checksum"
expect_rejected 'wrong filename'
printf '%s  %s\n%s  %s\n' "$fixture_hash" "$test_name" "$fixture_hash" "$test_name" > "$test_checksum"
expect_rejected 'duplicate checksum records'
printf 'invalid  %s\n' "$test_name" > "$test_checksum"
expect_rejected 'malformed hash'
: > "$test_checksum"
expect_rejected 'empty checksum'
rm -f "$test_checksum"
expect_rejected 'missing checksum'
printf 'PASS: shell installer checksum verification (8 cases)\n'

# 执行安装器原有下载替换代码，网络和服务操作替换为离线测试桩。
download_block=$(sed -n '/^stage_dir=$(mktemp -d /,/^# Detect init system and configure service/p' "$installer" | sed '$d')
[ -n "$download_block" ]
log_step() { :; }
log_info() { :; }
log_success() { :; }
uninstall_previous() { : > "$test_dir/service-stopped"; }
curl() {
    curl_output=""
    curl_url=""
    while [ "$#" -gt 0 ]; do
        case "$1" in
            -o) curl_output=$2; shift 2 ;;
            https://*) curl_url=$1; shift ;;
            *) shift ;;
        esac
    done
    case "$curl_url" in
        *.sha256)
            [ "$mock_mode" != missing ] || return 22
            if [ "$mock_mode" = mismatch ]; then
                printf '%064d  %s\n' 0 "$test_name" > "$curl_output"
            else
                printf '%s  %s\n' "$fixture_hash" "$test_name" > "$curl_output"
            fi
            ;;
        *) printf 'owned release\n' > "$curl_output" ;;
    esac
}

target_dir=$test_dir
file_name=$test_name
komari_agent_path="$test_dir/agent"
release_repository="wugan666/komari-agent"
version_to_install="v1.0.0-owned.1"
download_url="https://github.com/$release_repository/releases/download/$version_to_install/$file_name"
service_user=root
if [ -z "${EUID:-}" ]; then EUID=$(id -u); fi
GREEN="" CYAN="" NC=""
for mock_mode in missing mismatch valid; do
    printf 'existing release\n' > "$komari_agent_path"
    rm -f "$test_dir/service-stopped"
    if (eval "$download_block") >/dev/null 2>&1; then
        [ "$mock_mode" = valid ]
        [ "$(cat "$komari_agent_path")" = 'owned release' ]
        [ -f "$test_dir/service-stopped" ]
    else
        [ "$mock_mode" != valid ]
        [ "$(cat "$komari_agent_path")" = 'existing release' ]
        [ ! -e "$test_dir/service-stopped" ]
    fi
done
printf 'PASS: shell installer replacement ordering (3 cases)\n'
