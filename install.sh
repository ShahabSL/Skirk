#!/bin/sh
set -eu

repo="${SKIRK_REPO:-ShahabSL/Skirk}"
version="${SKIRK_VERSION:-latest}"
asset_base="${SKIRK_ASSET_BASE:-}"
require_release_asset="${SKIRK_REQUIRE_RELEASE_ASSET:-}"
dev_install=""
server_setup="${SKIRK_SERVER_SETUP:-}"
adc_path="${SKIRK_ADC:-}"
setup_out="${SKIRK_SETUP_OUT:-skirk-kit}"
install_systemd="${SKIRK_INSTALL_SYSTEMD:-}"
service_name="${SKIRK_SERVICE_NAME:-skirk-exit}"
service_user="${SKIRK_SERVICE_USER:-}"
exit_proxy="${SKIRK_EXIT_PROXY:-}"
install_wireproxy="${SKIRK_INSTALL_WIREPROXY:-}"
wireproxy_bind="${SKIRK_WIREPROXY_BIND:-127.0.0.1:40000}"
wireproxy_dir="${SKIRK_WIREPROXY_DIR:-/etc/wireproxy}"
wireproxy_bin="${SKIRK_WIREPROXY_BIN:-/usr/local/bin/wireproxy}"
wireproxy_user="${SKIRK_WIREPROXY_USER:-skirk-wireproxy}"
wireproxy_version="${SKIRK_WIREPROXY_VERSION:-v1.1.2}"
wgcf_bin="${SKIRK_WGCF_BIN:-/usr/local/bin/wgcf}"
wgcf_version="${SKIRK_WGCF_VERSION:-v2.2.30}"
wireproxy_only="${SKIRK_WIREPROXY_ONLY:-}"

default_install_dir() {
  if [ -n "${SKIRK_INSTALL_DIR:-}" ]; then
    printf '%s\n' "$SKIRK_INSTALL_DIR"
    return 0
  fi
  printf '%s\n' "$HOME/.local/bin"
}

install_dir="$(default_install_dir)"

usage() {
  cat <<EOF
Usage: install.sh [--version VERSION] [--wireproxy-only] [--dev-install] [uninstall|--uninstall]

Environment:
  SKIRK_VERSION=latest|vX.Y.Z  Select the Skirk release.
  SKIRK_INSTALL_DIR=PATH       Install directory. Default: \$HOME/.local/bin.
  SKIRK_REQUIRE_RELEASE_ASSET=1 Fail instead of building from source if release assets are unavailable.
                                  Defaults to 1 for explicit vX.Y.Z versions.
  SKIRK_INSTALL_WIREPROXY=1    Install Skirk-managed WARP wireproxy.
  --dev-install                Allow custom SKIRK_REPO/SKIRK_ASSET_BASE and source refs.
EOF
}

validate_version() {
  value="$1"
  case "$value" in
    ""|-*|*/*|*..*|*[!A-Za-z0-9._-]*)
      echo "error: unsafe version/ref: $value" >&2
      exit 1
      ;;
  esac
  case "$value" in
    v*)
      if ! is_release_version "$value"; then
        echo "error: release versions must use vX.Y.Z format: $value" >&2
        exit 1
      fi
      ;;
    latest) ;;
    *)
      if [ "$dev_install" != "1" ]; then
        echo "error: version must be latest or vX.Y.Z (use --dev-install for source refs)" >&2
        exit 1
      fi
      ;;
  esac
}

is_release_version() {
  value="$1"
  case "$value" in
    v*.*.*) ;;
    *) return 1 ;;
  esac
  rest="${value#v}"
  old_ifs="$IFS"
  IFS=.
  # shellcheck disable=SC2086
  set -- $rest
  IFS="$old_ifs"
  [ "$#" -eq 3 ] || return 1
  for part in "$@"; do
    case "$part" in
      ""|*[!0-9]*) return 1 ;;
    esac
  done
  return 0
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --version)
        [ "$#" -ge 2 ] || { echo "error: --version needs a value" >&2; exit 1; }
        version="$2"
        shift 2
        ;;
      --version=*)
        version="${1#--version=}"
        shift
        ;;
      --wireproxy-only)
        wireproxy_only=1
        install_wireproxy=1
        shift
        ;;
      --dev-install)
        dev_install=1
        shift
        ;;
      --help|-h)
        usage
        exit 0
        ;;
      *)
        break
        ;;
    esac
  done
  validate_version "$version"
  set -- "$@"
  parsed_args="$*"
}

normalize_service_unit() {
  name="$1"
  case "$name" in
    ""|*/*|*..*|*" "*|*"'"*|*'"'*|*\\*)
      echo "error: unsafe service name: $name" >&2
      exit 1
      ;;
  esac
  case "$name" in
    *.service) printf '%s\n' "$1" ;;
    *) printf '%s.service\n' "$1" ;;
  esac
}

is_skirk_unit_file() {
  unit_path="$1"
  [ -f "$unit_path" ] || return 1
  grep -Eq 'Managed by Skirk|Wireproxy WARP SOCKS proxy for Skirk exit|ExecStart=.* serve-exit .* --config ' "$unit_path"
}

is_skirk_dropin_file() {
  dropin_path="$1"
  [ -f "$dropin_path" ] || return 1
  if run_root grep -Fq "Managed by Skirk" "$dropin_path"; then
    return 0
  fi
  cleaned="$(run_root sed '/^[[:space:]]*$/d' "$dropin_path")"
  [ "$cleaned" = "[Unit]
After=wireproxy.service
Wants=wireproxy.service" ]
}

remove_skirk_wireproxy_dropin() {
  dropin_dir="$1"
  dropin_path="$dropin_dir/10-wireproxy.conf"
  if [ -e "$dropin_path" ]; then
    if ! is_skirk_dropin_file "$dropin_path"; then
      echo "error: refusing to remove non-Skirk drop-in: $dropin_path" >&2
      exit 1
    fi
    run_root rm -f "$dropin_path"
  fi
  run_root rmdir "$dropin_dir" 2>/dev/null || true
}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: missing required command: $1" >&2
    exit 1
  }
}

detect_platform() {
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$os" in
    linux) os="linux" ;;
    *) echo "error: this installer currently supports Linux exit/client machines only (got $os)" >&2; exit 1 ;;
  esac
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) echo "error: unsupported CPU architecture: $arch" >&2; exit 1 ;;
  esac
  echo "$os-$arch"
}

download() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    echo "error: install curl or wget" >&2
    exit 1
  fi
}

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    echo "error: this step needs root or sudo: $*" >&2
    exit 1
  fi
}

validate_wireproxy_bind() {
  case "$wireproxy_bind" in
    127.0.0.1:*) ;;
    *)
      echo "error: SKIRK_WIREPROXY_BIND must bind loopback only, for example 127.0.0.1:40000" >&2
      exit 1
      ;;
  esac
  case "$wireproxy_bind" in
    *[!0123456789.:]*|*:*:*)
      echo "error: SKIRK_WIREPROXY_BIND must be exactly 127.0.0.1:PORT" >&2
      exit 1
      ;;
  esac
  port="${wireproxy_bind#127.0.0.1:}"
  case "$port" in
    ""|*[!0-9]*)
      echo "error: SKIRK_WIREPROXY_BIND port is invalid: $wireproxy_bind" >&2
      exit 1
      ;;
  esac
  if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    echo "error: SKIRK_WIREPROXY_BIND port is out of range: $wireproxy_bind" >&2
    exit 1
  fi
}

validate_wireproxy_user() {
  if [ "$wireproxy_user" != "skirk-wireproxy" ]; then
    echo "error: managed WARP wireproxy must run as skirk-wireproxy" >&2
    exit 1
  fi
}

require_systemd_for_wireproxy() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "error: systemd is required to install and start WARP wireproxy" >&2
    exit 1
  fi
  if ! systemctl show --property=SystemState >/dev/null 2>&1; then
    echo "error: systemd is not usable on this machine" >&2
    exit 1
  fi
}

preflight_wireproxy_install() {
  if [ "$install_wireproxy" != "1" ]; then
    return 0
  fi
  validate_wireproxy_bind
  validate_wireproxy_user
  require_systemd_for_wireproxy
  need ss
  if [ "$wireproxy_dir" != "/etc/wireproxy" ] || [ "$wireproxy_bin" != "/usr/local/bin/wireproxy" ] || [ "$wgcf_bin" != "/usr/local/bin/wgcf" ]; then
    echo "error: managed WARP wireproxy uses fixed paths under /etc/wireproxy and /usr/local/bin" >&2
    exit 1
  fi
  if [ -L "$wireproxy_dir" ] || [ -L "$wireproxy_bin" ] || [ -L "$wgcf_bin" ]; then
    echo "error: refusing to use symlinked WARP wireproxy paths" >&2
    exit 1
  fi
  wireproxy_unit="/etc/systemd/system/wireproxy.service"
  if [ -e "$wireproxy_unit" ] && ! is_skirk_unit_file "$wireproxy_unit"; then
    echo "error: refusing to overwrite non-Skirk $wireproxy_unit" >&2
    exit 1
  fi
  manifest_ok=0
  if has_skirk_wireproxy_manifest; then
    verify_skirk_wireproxy_manifest
    manifest_ok=1
  fi
  if [ -e "$wireproxy_dir" ] && [ "$manifest_ok" != "1" ] && ! is_legacy_skirk_wireproxy_install; then
    echo "error: refusing to overwrite non-Skirk $wireproxy_dir" >&2
    exit 1
  fi
  if [ -e "$wireproxy_bin" ] && [ "$manifest_ok" != "1" ] && ! is_legacy_skirk_wireproxy_install; then
    echo "error: refusing to overwrite existing $wireproxy_bin without Skirk manifest" >&2
    exit 1
  fi
  if [ -e "$wgcf_bin" ] && [ "$manifest_ok" != "1" ] && ! is_legacy_skirk_wireproxy_install; then
    echo "error: refusing to overwrite existing $wgcf_bin without Skirk manifest" >&2
    exit 1
  fi
  port="${wireproxy_bind#127.0.0.1:}"
  if run_root ss -ltnp 2>/dev/null | awk -v port=":$port" '$4 ~ port "$" && $0 !~ /wireproxy/ { found = 1 } END { exit !found }'; then
    echo "error: $wireproxy_bind is already used by a non-wireproxy process" >&2
    exit 1
  fi
}

wait_wireproxy_ready() {
  port="${wireproxy_bind#127.0.0.1:}"
  i=0
  while [ "$i" -lt 30 ]; do
    if systemctl is-active --quiet wireproxy.service; then
      main_pid="$(systemctl show -p MainPID --value wireproxy.service 2>/dev/null || true)"
      if [ -n "$main_pid" ] && [ "$main_pid" != "0" ]; then
        if run_root ss -ltnp 2>/dev/null | awk -v want="127.0.0.1:$port" -v pid="$main_pid" '$4 == want && $0 ~ ("pid=" pid "([^0-9]|$)") { found = 1 } END { exit !found }'; then
          return 0
        fi
      fi
    fi
    sleep 1
    i=$((i + 1))
  done
  echo "error: wireproxy.service started but $wireproxy_bind did not become ready" >&2
  systemctl status wireproxy.service --no-pager >&2 || true
  exit 1
}

file_sha256() {
  path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$path" | awk '{print $1}'
  else
    echo "error: missing required command: sha256sum or shasum" >&2
    exit 1
  fi
}

root_file_exists() {
  run_root test -f "$1" >/dev/null 2>&1
}

has_skirk_wireproxy_manifest() {
  root_file_exists "$wireproxy_dir/skirk-managed.manifest"
}

wireproxy_manifest_value() {
  key="$1"
  run_root awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; found = 1; exit } END { exit found ? 0 : 1 }' "$wireproxy_dir/skirk-managed.manifest"
}

verify_manifest_checksum() {
  path="$1"
  want="$2"
  if [ -z "$want" ]; then
    echo "error: refusing to trust WARP manifest with empty checksum for $path" >&2
    exit 1
  fi
  if [ -e "$path" ]; then
    got="$(file_sha256 "$path")"
    if [ "$got" != "$want" ]; then
      echo "error: refusing to overwrite $path: WARP manifest checksum mismatch" >&2
      exit 1
    fi
  fi
}

verify_skirk_wireproxy_manifest() {
  manifest="$wireproxy_dir/skirk-managed.manifest"
  root_file_exists "$manifest" || {
    echo "error: missing WARP ownership manifest: $manifest" >&2
    exit 1
  }
  run_root grep -Fq "Managed by Skirk" "$manifest" || {
    echo "error: refusing to trust WARP manifest without Skirk marker: $manifest" >&2
    exit 1
  }
  m_dir="$(wireproxy_manifest_value config_dir || true)"
  m_wireproxy="$(wireproxy_manifest_value wireproxy_bin || true)"
  m_wireproxy_sha="$(wireproxy_manifest_value wireproxy_sha256 || true)"
  m_wgcf="$(wireproxy_manifest_value wgcf_bin || true)"
  m_wgcf_sha="$(wireproxy_manifest_value wgcf_sha256 || true)"
  m_service="$(wireproxy_manifest_value service || true)"
  m_user="$(wireproxy_manifest_value user || true)"
  if [ "$m_dir" != "$wireproxy_dir" ] || [ "$m_wireproxy" != "$wireproxy_bin" ] || [ "$m_wgcf" != "$wgcf_bin" ] || [ "$m_service" != "/etc/systemd/system/wireproxy.service" ] || [ "$m_user" != "$wireproxy_user" ]; then
    echo "error: refusing to trust WARP manifest with unexpected paths or user" >&2
    exit 1
  fi
  verify_manifest_checksum "$wireproxy_bin" "$m_wireproxy_sha"
  verify_manifest_checksum "$wgcf_bin" "$m_wgcf_sha"
}

is_legacy_skirk_wireproxy_install() {
  wireproxy_unit="/etc/systemd/system/wireproxy.service"
  [ -e "$wireproxy_unit" ] || return 1
  is_skirk_unit_file "$wireproxy_unit" || return 1
  root_file_exists "$wireproxy_dir/wireproxy.conf" || return 1
  run_root grep -Fq "WGConfig = $wireproxy_dir/wgcf-profile.conf" "$wireproxy_dir/wireproxy.conf"
}

ensure_wireproxy_user() {
  if command -v getent >/dev/null 2>&1 && getent passwd "$wireproxy_user" >/dev/null 2>&1; then
    user_entry="$(getent passwd "$wireproxy_user")"
    uid="$(printf '%s\n' "$user_entry" | awk -F: '{print $3}')"
    shell="$(printf '%s\n' "$user_entry" | awk -F: '{print $7}')"
    if [ "$uid" -eq 0 ]; then
      echo "error: refusing to run WARP wireproxy as uid 0" >&2
      exit 1
    fi
    case "$shell" in
      */nologin|*/false) ;;
      *)
        echo "error: refusing to reuse existing $wireproxy_user account with login shell $shell" >&2
        exit 1
        ;;
    esac
    return 0
  fi
  command -v useradd >/dev/null 2>&1 || {
    echo "error: useradd is required to create $wireproxy_user" >&2
    exit 1
  }
  nologin_shell="/usr/sbin/nologin"
  [ -x "$nologin_shell" ] || nologin_shell="/sbin/nologin"
  [ -x "$nologin_shell" ] || nologin_shell="/bin/false"
  run_root useradd --system --no-create-home --shell "$nologin_shell" "$wireproxy_user"
}

uninstall_skirk() {
  remove_service=1
  remove_binary=1
  dry_run=0
  for arg in "$@"; do
    case "$arg" in
      --service=false) remove_service=0 ;;
      --binary=false) remove_binary=0 ;;
      --dry-run) dry_run=1 ;;
    esac
  done
  service_unit="$(normalize_service_unit "$service_name")"
  skirk_bin="$install_dir/skirk"
  if [ -x "$skirk_bin" ]; then
    "$skirk_bin" uninstall --yes --name "$service_name" --bin "$skirk_bin" "$@"
    return 0
  fi

  echo "Skirk binary was not found at $skirk_bin; running fallback uninstall."
  if command -v systemctl >/dev/null 2>&1 && [ "$remove_service" = "1" ]; then
    unit_path="/etc/systemd/system/$service_unit"
    if [ -e "$unit_path" ]; then
      if ! is_skirk_unit_file "$unit_path"; then
        echo "error: refusing to remove $unit_path because it is not managed by Skirk" >&2
        exit 1
      fi
      if [ "$dry_run" = "1" ]; then
        echo "Would remove systemd service: $service_unit"
      else
        run_root systemctl disable --now "$service_unit" 2>/dev/null || true
        run_root rm -f "$unit_path"
        run_root systemctl daemon-reload || true
        echo "Removed systemd service: $service_unit"
      fi
    else
      echo "Systemd service file already absent: $unit_path"
    fi
  fi
  if [ "$remove_binary" = "1" ]; then
    if [ "$dry_run" = "1" ]; then
      echo "Would remove installed binary: $skirk_bin"
    else
      rm -f "$skirk_bin"
      echo "Removed installed binary: $skirk_bin"
    fi
  fi
  echo "Skirk uninstall complete."
}

install_from_release() {
  platform="$1"
  tmp="$2"
  asset="skirk-$platform.tar.gz"
  if [ -n "$asset_base" ]; then
    url="$asset_base/$asset"
  elif [ "$version" = "latest" ]; then
    url="https://github.com/$repo/releases/latest/download/$asset"
  else
    url="https://github.com/$repo/releases/download/$version/$asset"
  fi

  echo "Downloading $url"
  if ! download "$url" "$tmp/$asset"; then
    return 1
  fi
  tar -xzf "$tmp/$asset" -C "$tmp"
  if [ ! -x "$tmp/skirk" ]; then
    echo "error: release archive did not contain executable skirk" >&2
    return 1
  fi
  verify_candidate_version "$tmp/skirk"
  mkdir -p "$install_dir"
  install_tmp="$install_dir/.skirk.tmp.$$"
  cp "$tmp/skirk" "$install_tmp"
  chmod 0755 "$install_tmp"
  mv "$install_tmp" "$install_dir/skirk"
  verify_candidate_version "$install_dir/skirk"
}

install_warp_wireproxy() {
  if [ "$install_wireproxy" != "1" ]; then
    return 0
  fi
  need tar

  case "$platform" in
    linux-amd64|linux-arm64) ;;
    *) echo "error: WARP wireproxy installer supports linux-amd64 and linux-arm64 only (got $platform)" >&2; exit 1 ;;
  esac
  preflight_wireproxy_install

  if [ -z "$exit_proxy" ]; then
    exit_proxy="socks5h://$wireproxy_bind"
  fi

  tmp_warp="$tmp/warp"
  mkdir -p "$tmp_warp"
  wgcf_arch="${platform#linux-}"
  wgcf_no_v="${wgcf_version#v}"
  wgcf_url="https://github.com/ViRb3/wgcf/releases/download/$wgcf_version/wgcf_${wgcf_no_v}_linux_${wgcf_arch}"
  wireproxy_url="https://github.com/windtf/wireproxy/releases/download/$wireproxy_version/wireproxy_linux_${wgcf_arch}.tar.gz"

  echo "Installing WARP wireproxy..."
  ensure_wireproxy_user
  download "$wgcf_url" "$tmp_warp/wgcf"
  chmod 0755 "$tmp_warp/wgcf"

  download "$wireproxy_url" "$tmp_warp/wireproxy.tar.gz"
  mkdir "$tmp_warp/wireproxy"
  tar -xzf "$tmp_warp/wireproxy.tar.gz" -C "$tmp_warp/wireproxy"
  wireproxy_src="$(find "$tmp_warp/wireproxy" -type f -name wireproxy -perm -111 | head -n 1)"
  if [ -z "$wireproxy_src" ]; then
    echo "error: wireproxy release archive did not contain executable wireproxy" >&2
    exit 1
  fi

  run_root mkdir -p "$wireproxy_dir"
  run_root chmod 0711 "$wireproxy_dir"
  if ! root_file_exists "$wireproxy_dir/wgcf-account.toml" || [ "${SKIRK_RESET_WARP:-}" = "1" ]; then
    (
      cd "$tmp_warp"
      if [ "${SKIRK_ACCEPT_WARP_TOS:-}" = "1" ]; then
        printf "yes\n" | "$tmp_warp/wgcf" register
      else
        "$tmp_warp/wgcf" register
      fi
      "$tmp_warp/wgcf" generate --profile "$tmp_warp/wgcf-profile.conf"
    )
    run_root install -m 0600 "$tmp_warp/wgcf-account.toml" "$wireproxy_dir/wgcf-account.toml"
    run_root install -m 0600 "$tmp_warp/wgcf-profile.conf" "$wireproxy_dir/wgcf-profile.conf"
  elif ! root_file_exists "$wireproxy_dir/wgcf-profile.conf"; then
    run_root cp "$wireproxy_dir/wgcf-account.toml" "$tmp_warp/wgcf-account.toml"
    run_root chown "$(id -u):$(id -g)" "$tmp_warp/wgcf-account.toml"
    (
      cd "$tmp_warp"
      "$tmp_warp/wgcf" generate --profile "$tmp_warp/wgcf-profile.conf"
    )
    run_root install -m 0600 "$tmp_warp/wgcf-profile.conf" "$wireproxy_dir/wgcf-profile.conf"
  fi

  wireproxy_conf="$tmp_warp/wireproxy.conf"
  cat >"$wireproxy_conf" <<EOF
WGConfig = $wireproxy_dir/wgcf-profile.conf

[Socks5]
BindAddress = $wireproxy_bind
EOF
  wireproxy_sha="$(file_sha256 "$wireproxy_src")"
  wgcf_sha="$(file_sha256 "$tmp_warp/wgcf")"
  wireproxy_manifest="$tmp_warp/skirk-managed.manifest"
  cat >"$wireproxy_manifest" <<EOF
# Managed by Skirk
config_dir=$wireproxy_dir
wireproxy_bin=$wireproxy_bin
wireproxy_sha256=$wireproxy_sha
wgcf_bin=$wgcf_bin
wgcf_sha256=$wgcf_sha
service=/etc/systemd/system/wireproxy.service
user=$wireproxy_user
EOF
  run_root install -m 0755 "$tmp_warp/wgcf" "$wgcf_bin"
  run_root install -m 0755 "$wireproxy_src" "$wireproxy_bin"
  run_root install -m 0644 "$wireproxy_conf" "$wireproxy_dir/wireproxy.conf"
  run_root install -m 0644 "$wireproxy_manifest" "$wireproxy_dir/skirk-managed.manifest"
  run_root chown root:root "$wireproxy_dir" "$wireproxy_dir/wireproxy.conf" "$wireproxy_dir/skirk-managed.manifest"
  run_root chmod 0711 "$wireproxy_dir"
  run_root chown "$wireproxy_user" "$wireproxy_dir/wgcf-account.toml" "$wireproxy_dir/wgcf-profile.conf"
  run_root chmod 0600 "$wireproxy_dir/wgcf-account.toml" "$wireproxy_dir/wgcf-profile.conf"
  wireproxy_unit="$tmp_warp/wireproxy.service"
  cat >"$wireproxy_unit" <<EOF
[Unit]
Description=Wireproxy WARP SOCKS proxy for Skirk exit
# Managed by Skirk
After=network-online.target
Wants=network-online.target

[Service]
Type=exec
User=$wireproxy_user
ExecStart=$wireproxy_bin -c $wireproxy_dir/wireproxy.conf
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
ReadOnlyPaths=$wireproxy_dir

[Install]
WantedBy=multi-user.target
EOF
  if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze verify "$wireproxy_unit"
  fi
  run_root install -m 0644 "$wireproxy_unit" /etc/systemd/system/wireproxy.service
  run_root systemctl daemon-reload
  run_root systemctl enable wireproxy.service
  run_root systemctl restart wireproxy.service
  wait_wireproxy_ready

  echo "WARP wireproxy listening on $wireproxy_bind"
}

install_from_source() {
  tmp="$1"
  need go
  ref="$version"
  if [ "$ref" = "latest" ]; then
    ref="main"
  fi
  url="https://github.com/$repo/archive/refs/heads/$ref.tar.gz"
  case "$ref" in
    v*) url="https://github.com/$repo/archive/refs/tags/$ref.tar.gz" ;;
  esac

  echo "Release asset unavailable; building from source at $url"
  download "$url" "$tmp/source.tar.gz"
  mkdir "$tmp/source"
  tar -xzf "$tmp/source.tar.gz" -C "$tmp/source" --strip-components 1
  mkdir -p "$install_dir"
  build_out="$tmp/skirk-built"
  (
    cd "$tmp/source"
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$build_out" ./cmd/skirk
  )
  install_tmp="$install_dir/.skirk.tmp.$$"
  cp "$build_out" "$install_tmp"
  chmod 0755 "$install_tmp"
  mv "$install_tmp" "$install_dir/skirk"
}

verify_candidate_version() {
  binary="$1"
  if ! is_release_version "$version"; then
    return 0
  fi
  installed_output="$("$binary" version)"
  installed="${installed_output#skirk }"
  installed="${installed%% *}"
  if [ "$installed" != "$version" ]; then
    echo "error: candidate skirk reports $installed, expected $version" >&2
    exit 1
  fi
}

run_server_setup() {
  if [ "$server_setup" != "1" ]; then
    return 0
  fi

  set -- setup init --out "$setup_out"
  if [ -n "$adc_path" ]; then
    set -- "$@" --adc "$adc_path"
  fi
  if [ -n "${SKIRK_OAUTH_CLIENT_FILE:-}" ]; then
    set -- "$@" --oauth-client-file "$SKIRK_OAUTH_CLIENT_FILE"
  fi
  if [ "${SKIRK_RESET_GOOGLE_LOGIN:-}" = "1" ]; then
    set -- "$@" --reset-google-login
  fi
  if [ -n "${SKIRK_CLIENT_ROUTE:-}" ]; then
    set -- "$@" --client-route "$SKIRK_CLIENT_ROUTE"
  fi
  if [ -n "${SKIRK_EXIT_ROUTE:-}" ]; then
    set -- "$@" --exit-route "$SKIRK_EXIT_ROUTE"
  fi
  if [ -n "$exit_proxy" ]; then
    set -- "$@" --exit-proxy "$exit_proxy"
  fi

  echo
  echo "Running server setup..."
  "$install_dir/skirk" "$@"

  if [ "$install_systemd" = "1" ]; then
    install_exit_service
  fi
}

install_exit_service() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "Note: systemd is not available; start the exit manually with: $install_dir/skirk serve-exit --config $setup_out/exit.json" >&2
    return 0
  fi
  if [ -z "$service_user" ]; then
    service_user="$(id -un)"
  fi
  config_path="$setup_out/exit.json"
  case "$config_path" in
    /*) ;;
    *) config_path="$(pwd)/$config_path" ;;
  esac
  service_unit="$(normalize_service_unit "$service_name")"
  unit="/etc/systemd/system/$service_unit"
  if [ -e "$unit" ] && ! is_skirk_unit_file "$unit"; then
    echo "error: refusing to overwrite non-Skirk systemd service: $unit" >&2
    exit 1
  fi
  tmp_unit="$(mktemp)"
  cat >"$tmp_unit" <<EOF
[Unit]
Description=Skirk exit
# Managed by Skirk
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$service_user
WorkingDirectory=$(dirname "$config_path")
ExecStart=$install_dir/skirk serve-exit --config $config_path
Restart=always
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
  if [ "$(id -u)" -eq 0 ]; then
    cp "$tmp_unit" "$unit"
    remove_skirk_wireproxy_dropin "$unit.d"
    if [ "$install_wireproxy" = "1" ]; then
      mkdir -p "$unit.d"
      cat >"$unit.d/10-wireproxy.conf" <<EOF
[Unit]
# Managed by Skirk
After=wireproxy.service
Wants=wireproxy.service
EOF
    fi
    systemctl daemon-reload
    systemctl enable --now "$service_unit"
  elif command -v sudo >/dev/null 2>&1; then
    sudo cp "$tmp_unit" "$unit"
    remove_skirk_wireproxy_dropin "$unit.d"
    if [ "$install_wireproxy" = "1" ]; then
      tmp_dropin="$(mktemp)"
      cat >"$tmp_dropin" <<EOF
[Unit]
# Managed by Skirk
After=wireproxy.service
Wants=wireproxy.service
EOF
      sudo mkdir -p "$unit.d"
      sudo cp "$tmp_dropin" "$unit.d/10-wireproxy.conf"
      rm -f "$tmp_dropin"
    fi
    sudo systemctl daemon-reload
    sudo systemctl enable --now "$service_unit"
  else
    echo "Note: sudo is unavailable; systemd service file was left at $tmp_unit" >&2
    return 0
  fi
  rm -f "$tmp_unit"
  echo "Installed and started systemd service: $service_unit"
}

main() {
  parse_args "$@"
  if [ "$dev_install" != "1" ]; then
    repo="ShahabSL/Skirk"
    asset_base=""
  fi
  if [ -z "$require_release_asset" ]; then
    require_release_asset=1
  fi
  # shellcheck disable=SC2086
  set -- $parsed_args
  if [ "${1:-}" = "uninstall" ] || [ "${1:-}" = "--uninstall" ]; then
    case "${1:-}" in
      uninstall|--uninstall) shift ;;
    esac
    uninstall_skirk "$@"
    return 0
  fi

  need uname
  need tar
  platform="$(detect_platform)"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT INT TERM
  preflight_wireproxy_install

  if [ "$wireproxy_only" = "1" ]; then
    install_warp_wireproxy
    return 0
  fi

  if ! install_from_release "$platform" "$tmp"; then
    if [ "$require_release_asset" = "1" ] || [ "$dev_install" != "1" ]; then
      echo "error: release asset unavailable for $version; refusing source fallback" >&2
      exit 1
    fi
    install_from_source "$tmp"
  fi
  install_warp_wireproxy

  echo
  "$install_dir/skirk" version
  echo "Installed: $install_dir/skirk"
  case ":$PATH:" in
    *":$install_dir:"*) ;;
    *) echo "Current shell may not find skirk yet. Run: export PATH=\"$install_dir:\$PATH\"" ;;
  esac
  echo
  echo "Next: run '$install_dir/skirk' for guided setup, including easy or personal OAuth."
  echo "Direct setup is also available: '$install_dir/skirk setup init --out skirk-kit --reset-google-login'."

  run_server_setup
}

main "$@"
