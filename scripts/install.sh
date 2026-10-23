#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'install: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: scripts/install.sh [--yes]

Build and install the sam-g binary from this checkout. Create the example
configuration only when ~/.sam-g/config.json is absent. The binary publishes its
bundled docs on the first normal start. Never starts sam-g.
  --yes   Confirm the install explicitly (required when stdin/stdout is not a TTY).
EOF
}

assume_yes=false
while (($#)); do
  case "$1" in
    --yes) assume_yes=true ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "unknown option: $1" ;;
  esac
  shift
done

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) fail "sam-g supports macOS and Linux only" ;;
esac

command -v go >/dev/null 2>&1 || fail "Go 1.27 or newer is required"
go_version=$(go env GOVERSION) || fail "could not determine the Go version"
if [[ $go_version =~ ^go([0-9]+)\.([0-9]+) ]]; then
  go_major=${BASH_REMATCH[1]}
  go_minor=${BASH_REMATCH[2]}
  if (( go_major < 1 || (go_major == 1 && go_minor < 27) )); then
    fail "Go 1.27 or newer is required (found $go_version)"
  fi
else
  fail "could not determine the Go version from '$go_version'"
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd "$script_dir/.." && pwd)
sam_g_dir=${HOME:?HOME must be set}/.sam-g
bin_dir=$sam_g_dir/bin
logs_dir=$sam_g_dir/logs
binary=$bin_dir/sam-g
config=$sam_g_dir/config.json
template=$repo_dir/docs/sam-g.example.json

[[ -f $template ]] || fail "example config not found at $template; run this from a sam-g checkout"

if [[ $assume_yes != true ]]; then
  if [[ ! -t 0 || ! -t 1 ]]; then
    fail "confirmation needs a terminal; review the install with scripts/install.sh --help, then pass --yes explicitly to continue"
  fi
  cat >&2 <<EOF
This will build the current checkout and install its binary at:
  $binary
It will create private ~/.sam-g directories and copy the example config to:
  $config
only if that config does not already exist. An existing config is preserved.
The binary publishes its version-matched docs under ~/.sam-g/docs on first start.
It will not start sam-g, configure Tailscale, or install reboot supervision.
Continue with this install? [y/N]
EOF
  IFS= read -r answer || fail "could not read confirmation"
  case "$answer" in
    y|Y|yes|YES|Yes) ;;
    *) printf 'Install cancelled; no files were changed.\n' >&2; exit 0 ;;
  esac
fi

umask 077
mkdir -p "$bin_dir" "$logs_dir"
chmod 700 "$sam_g_dir" "$bin_dir" "$logs_dir"

tmp_binary=$(mktemp "$bin_dir/.sam-g.XXXXXXXX") || fail "could not create a temporary binary in $bin_dir"
trap 'rm -f "$tmp_binary"' EXIT
(
  cd "$repo_dir"
  go build -o "$tmp_binary" ./cmd
) || fail "build failed; existing installed binary was left unchanged"
chmod 755 "$tmp_binary"
mv -f "$tmp_binary" "$binary"

if [[ -e $config || -L $config ]]; then
  printf 'Preserved existing config: %s\n' "$config"
else
  tmp_config=$(mktemp "$sam_g_dir/.config.json.XXXXXXXX") || fail "could not create a temporary config in $sam_g_dir"
  if ! cp "$template" "$tmp_config"; then
    rm -f "$tmp_config"
    fail "could not copy the example config"
  fi
  chmod 600 "$tmp_config"
  if ln "$tmp_config" "$config" 2>/dev/null; then
    rm -f "$tmp_config"
    printf 'Created config from example: %s\n' "$config"
  elif [[ -e $config || -L $config ]]; then
    rm -f "$tmp_config"
    printf 'Preserved existing config: %s\n' "$config"
  else
    rm -f "$tmp_config"
    fail "could not install the example config at $config"
  fi
fi

printf '\nInstalled sam-g: %s\n' "$binary"
printf 'Before running it, check the model settings in %s and export any API-key environment variable your provider requires.\n' "$config"
printf 'Version-matched bundled docs will be published on first start under: %s/docs\n' "$sam_g_dir"
printf 'Start it with: %s/run.sh\n' "$script_dir"
