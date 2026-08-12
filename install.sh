#!/usr/bin/env bash
#
# ============================================================================
# triproxy 一键安装脚本（部署为 Linux systemd 服务）
# ============================================================================
#
# 支持的操作系统（Linux + systemd）：
#   - Debian 10+ / Ubuntu 18.04+            (apt)
#   - RHEL 7+ / CentOS 7+ / Fedora           (yum/dnf)
#   - Rocky Linux / AlmaLinux                (dnf)
#   - Arch Linux / Manjaro                   (pacman)
#   - openSUSE Leap / Tumbleweed             (zypper)
#   - Alpine Linux 3.x                       (apk，需额外安装 bash)
#
# 支持的 CPU 架构（与 GitHub Release 提供的 Linux 二进制一致）：
#   - x86_64 / amd64
#   - aarch64 / arm64
#   - i386 / i686（32 位 x86）
#   - armv7l / armv6l（32 位 ARM，树莓派等）
#   - riscv64
#   - loongarch64（龙芯）
#   - ppc64le（POWER）
#   - s390x（IBM Z）
#   其他架构没有预编译二进制，本脚本会拒绝安装。
#
# 不支持的系统：
#   - macOS / Windows（没有 systemd，请直接运行对应平台的独立二进制）
#   - 未启用 systemd 的 Linux（容器/发行版），或 PID 1 非 systemd
#   - WSL1（WSL2 且启用 systemd 可用）
#
# 安装位置：
#   二进制   -> /usr/local/bin/triproxy
#   配置     -> /etc/triproxy/config.yaml
#   服务     -> /etc/systemd/system/triproxy.service
#   运行用户 -> triproxy（专用低权限用户）
#
# 用法：
#   ./install.sh            # 默认：从 GitHub Release 下载对应平台二进制并安装启动
#   ./install.sh --version v1.2.0   # 下载指定版本（默认 latest）
#   ./install.sh --local    # 使用本地放好的二进制（脚本同目录或 dist/ 下）
#   ./install.sh --yes      # 跳过确认提示
#   ./install.sh --dry-run  # 只打印将要执行的动作，不改动系统
#   ./install.sh --uninstall# 卸载（停服务 + 删二进制/服务文件）
# ============================================================================

set -euo pipefail

# ----------------------------------------------------------------------------
# 参数解析
# ----------------------------------------------------------------------------
DRY_RUN=0
ASSUME_YES=0
MODE="install"
LOCAL_MODE=0
VERSION="latest"
REPO="tomoncle/triproxy"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)  DRY_RUN=1 ;;
    --yes|-y)   ASSUME_YES=1 ;;
    --uninstall) MODE="uninstall" ;;
    --local)    LOCAL_MODE=1 ;;
    --version)  shift; [[ $# -gt 0 ]] || die "--version 需要一个版本号（如 v1.2.0）"; VERSION="$1" ;;
    -h|--help)  sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "未知参数: $1"; exit 2 ;;
  esac
  shift
done

BIN_PATH="/usr/local/bin/triproxy"
CONF_DIR="/etc/triproxy"
CONF_PATH="$CONF_DIR/config.yaml"
SVC_PATH="/etc/systemd/system/triproxy.service"
SVC_USER="triproxy"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "错误: $*" >&2; exit 1; }

# 下载模式的临时目录，脚本退出时自动清理
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

# 安装/卸载都要写 /usr/local/bin、/etc、建用户、改 systemd，必须 root
require_root() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    return 0 # dry-run 不改系统，任何用户都能预览
  fi
  if [[ "$(id -u)" -ne 0 ]]; then
    die "需要 root 权限，请用 sudo 运行: sudo $0 $*"
  fi
}

# 执行真实命令或 dry-run 打印
run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '  [dry-run] %q' "$1"; shift; printf ' %q' "$@"; echo
  else
    "$@"
  fi
}

confirm() {
  if [[ "$ASSUME_YES" -eq 1 || "$DRY_RUN" -eq 1 ]]; then
    return 0
  fi
  read -r -p "继续安装到 $BIN_PATH 和 $CONF_DIR ？[y/N] " ans
  [[ "$ans" == "y" || "$ans" == "Y" ]] || { echo "已取消。"; exit 0; }
}

# ----------------------------------------------------------------------------
# 1. 系统支持性检查（声明支持范围的落地检查）
# ----------------------------------------------------------------------------
check_supported() {
  local os arch
  os="$(uname -s)"
  if [[ "$os" != "Linux" ]]; then
    die "当前系统是 $os，本脚本仅支持 Linux（systemd）。macOS/Windows 请直接运行独立二进制。"
  fi

  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)   TARGET_ARCH="amd64" ;;
    aarch64|arm64)  TARGET_ARCH="arm64" ;;
    i386|i486|i586|i686|x86) TARGET_ARCH="386" ;;
    armv6l|armv7l|armv8l|arm) TARGET_ARCH="arm" ;;
    riscv64)        TARGET_ARCH="riscv64" ;;
    loongarch64|loong64) TARGET_ARCH="loong64" ;;
    ppc64le)        TARGET_ARCH="ppc64le" ;;
    s390x)          TARGET_ARCH="s390x" ;;
    *) die "不支持的架构: $arch（请从 GitHub Release 下载对应平台的独立二进制）" ;;
  esac

  # 必须跑在 systemd 上：检查 PID 1 或 systemctl 存在
  if [[ ! -d /run/systemd/system ]] && ! command -v systemctl >/dev/null 2>&1; then
    die "未检测到 systemd。本脚本仅支持 systemd 发行版（见脚本头部支持列表）。"
  fi
}

# 打印发行版信息（仅提示用，不阻断）
show_distro() {
  if [[ -r /etc/os-release ]]; then
    echo "检测到系统: $(grep -E '^(PRETTY_NAME)=' /etc/os-release | cut -d= -f2 | tr -d '"')"
  fi
  echo "检测到架构: $(uname -m) -> ${TARGET_ARCH}"
}

# 从 GitHub Release 下载一个附件到临时目录
gh_download() {
  local file="$1" url="https://github.com/${REPO}/releases/download/${VERSION}/$1"
  echo "  下载 $file (${VERSION}) ..."
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 2 --connect-timeout 15 --max-time 180 -sS -o "$TMP_DIR/$file" "$url" \
      || die "下载失败: $url（可检查 --version 或改用 --local）"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=15 --tries=2 -O "$TMP_DIR/$file" "$url" \
      || die "下载失败: $url（可检查 --version 或改用 --local）"
  else
    die "未找到 curl 或 wget，无法下载。请安装 curl/wget，或用 --local 使用本地二进制。"
  fi
}

# 定位本地文件：临时目录（下载模式）-> 脚本同目录 -> dist/
locate() {
  local f="$1"
  if [[ -f "$TMP_DIR/$f" ]]; then echo "$TMP_DIR/$f"; return 0; fi
  if [[ -f "$SCRIPT_DIR/$f" ]]; then echo "$SCRIPT_DIR/$f"; return 0; fi
  if [[ -f "$SCRIPT_DIR/dist/$f" ]]; then echo "$SCRIPT_DIR/dist/$f"; return 0; fi
  return 1
}

find_source_binary() {
  local name="triproxy-linux-${TARGET_ARCH}"
  if [[ "$LOCAL_MODE" -eq 1 ]]; then
    SRC_BIN="$(locate "$name")" || die "未找到本地二进制 $name（--local 模式：请把 $name 放在脚本同目录或 dist/ 下）"
  else
    echo "==> 从 GitHub Release 获取二进制 (repo=$REPO version=$VERSION)"
    gh_download "$name"
    chmod +x "$TMP_DIR/$name"
    SRC_BIN="$TMP_DIR/$name"
    # 同时下载配置模板与服务模板，本地已有则优先用本地
    gh_download config.yaml || true
    gh_download triproxy.service || true
  fi
}

# 创建专用低权限用户，兼容不同发行版：
#   优先 useradd -r（shadow/util-linux，Debian/RHEL/Arch...）
#   回退 useradd 去掉 -r，再回退 adduser（Alpine busybox）
create_user() {
  if id "$SVC_USER" >/dev/null 2>&1; then
    echo "  用户已存在，跳过创建。"
    return
  fi
  if [[ "$DRY_RUN" -eq 1 ]]; then
    run useradd -r -s /usr/sbin/nologin "$SVC_USER"
    return
  fi
  if command -v useradd >/dev/null 2>&1; then
    if useradd -r -s /usr/sbin/nologin "$SVC_USER" 2>/dev/null; then
      return
    fi
    if useradd -s /usr/sbin/nologin "$SVC_USER" 2>/dev/null; then
      return
    fi
  fi
  if command -v adduser >/dev/null 2>&1; then
    if adduser -S -s /sbin/nologin "$SVC_USER" 2>/dev/null; then
      return
    fi
    if adduser -s /sbin/nologin "$SVC_USER" 2>/dev/null; then
      return
    fi
  fi
  die "无法创建系统用户 $SVC_USER，请手动执行后重试: useradd -r -s /usr/sbin/nologin $SVC_USER"
}

# ----------------------------------------------------------------------------
# 2. 卸载
# ----------------------------------------------------------------------------
do_uninstall() {
  echo "== 卸载 triproxy =="
  require_root
  run systemctl disable --now triproxy.service || true
  run rm -f "$SVC_PATH"
  run rm -f "$BIN_PATH"
  run systemctl daemon-reload
  echo "已卸载二进制与服务文件。"
  echo "如需清理配置和用户（会删除 $CONF_DIR 和系统用户 $SVC_USER）："
  echo "  sudo rm -rf $CONF_DIR && sudo userdel $SVC_USER"
}

# ----------------------------------------------------------------------------
# 3. 安装
# ----------------------------------------------------------------------------
do_install() {
  echo "== triproxy 安装 =="
  require_root
  show_distro
  confirm
  find_source_binary
  echo "使用二进制: $SRC_BIN"

  # 3.1 部署二进制
  echo "[1/6] 部署二进制 -> $BIN_PATH"
  run install -D -m 0755 "$SRC_BIN" "$BIN_PATH"

  # 3.2 部署配置（不覆盖已有配置）
  echo "[2/6] 部署配置 -> $CONF_PATH"
  if [[ -f "$CONF_PATH" && "$DRY_RUN" -eq 0 ]]; then
    echo "  已存在 $CONF_PATH，跳过（如需更新请自行替换）。"
  else
    CONF_SRC="$(locate config.yaml)" \
      || die "未找到 config.yaml，请把配置模板与脚本放在同一目录，或使用默认下载模式。"
    run install -d -m 0755 "$CONF_DIR"
    run install -m 0644 "$CONF_SRC" "$CONF_PATH"
  fi

  # 3.3 创建专用用户（幂等）
  echo "[3/6] 运行用户 $SVC_USER"
  create_user
  run chown -R "$SVC_USER:$SVC_USER" "$CONF_DIR"

  # 3.4 安装 systemd 服务
  echo "[4/6] 安装 systemd 服务 -> $SVC_PATH"
  SVC_SRC="$(locate triproxy.service)" || die "未找到 triproxy.service，无法安装系统服务。"
  run install -m 0644 "$SVC_SRC" "$SVC_PATH"

  # 3.5 重载并启用
  echo "[5/6] 启用并启动服务"
  run systemctl daemon-reload
  run systemctl enable --now triproxy.service

  # 3.6 校验
  echo "[6/6] 校验"
  if [[ "$DRY_RUN" -eq 0 ]]; then
    sleep 1
    if systemctl is-active --quiet triproxy.service; then
      echo "  服务已运行 ✓  (systemctl status triproxy)"
      echo "  日志: journalctl -u triproxy -f"
      echo "  健康检查: curl http://localhost:8866/healthz"
    else
      echo "  服务未激活，请查看日志: journalctl -u triproxy -e"
    fi
  fi
}

# ----------------------------------------------------------------------------
check_supported

if [[ "$MODE" == "uninstall" ]]; then
  do_uninstall
else
  do_install
fi
