#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'run: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: scripts/run.sh [--bg] [--tailscale-service [NAME]] [--yes]

Start the installed sam-g server on this machine, in the foreground by default.
  --bg                       Detach from this terminal and append output to
                             ~/.sam-g/logs/server.log. Does not start at boot.
  --tailscale-service [NAME] Configure this machine as the host for exactly
                             svc:NAME, then start its local sam-g process.
                             If NAME is omitted, ask for one interactively.
                             Use a distinct name per machine/database, such as
                             sam-g-server or sam-g-laptop; dotted names are invalid.
  --yes                      Explicitly confirm service setup without prompting.
                             Required with a NAME in noninteractive use.
  -h, --help                 Show this help.

Service setup changes only this machine's Tailscale Services endpoint:
HTTPS port 443 -> the configured 127.0.0.1 sam-g listener. It does not use
ordinary device-level Serve, change another host, create the admin Service,
retag this device, modify grants or unrelated routes, use Funnel, or supervise reboot startup.
Tailscale Services requires Tailscale 1.86+, a tag-based host identity, HTTPS
support already enabled, an admin-defined Service, and host approval (or an
auto-approval policy) plus a separate access grant. The helper checks the local
version, tag, and HTTPS capability; admin-console tasks remain with an
administrator.

Requires jq only for --tailscale-service. See docs/README.md for setup details.
EOF
}

background=false
use_tailscale_service=false
assume_yes=false
service_option_seen=false
service_name=
while (($#)); do
  case "$1" in
    --bg) background=true ;;
    --tailscale-service)
      [[ $service_option_seen != true ]] || fail "specify --tailscale-service only once"
      service_option_seen=true
      use_tailscale_service=true
      if (($# > 1)) && [[ $2 != -* ]]; then
        service_name=$2
        shift
      fi
      ;;
    --tailscale-service=*)
      [[ $service_option_seen != true ]] || fail "specify --tailscale-service only once"
      service_option_seen=true
      use_tailscale_service=true
      service_name=${1#*=}
      ;;
    --yes) assume_yes=true ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "unknown option: $1" ;;
  esac
  shift
done

if [[ $assume_yes == true && $use_tailscale_service != true ]]; then
  fail "--yes only applies to --tailscale-service"
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
sam_g_dir=${HOME:?HOME must be set}/.sam-g
binary=$sam_g_dir/bin/sam-g
config=$sam_g_dir/config.json

if [[ ! -x $binary ]]; then
  printf 'Installed sam-g binary not found at %s.\n' "$binary" >&2
  printf 'Run the installer first: %s/install.sh\n' "$script_dir" >&2
  exit 1
fi
if [[ ! -f $config ]]; then
  printf 'Configuration not found at %s.\n' "$config" >&2
  printf 'Run the installer first, then edit the example configuration.\n' >&2
  exit 1
fi

service_target=
service_hostname=
if [[ $use_tailscale_service == true ]]; then
  if ! command -v jq >/dev/null 2>&1; then
    fail "--tailscale-service requires jq to read server.listen from $config without guessing; install jq and retry"
  fi
  if ! command -v tailscale >/dev/null 2>&1; then
    fail "--tailscale-service requires the Tailscale CLI; install and connect Tailscale on this machine, then retry"
  fi

  if [[ -z $service_name ]]; then
    if [[ ! -t 0 || ! -t 1 ]]; then
      fail "choose a local service name explicitly: --tailscale-service sam-g-server --yes (use a different name on each machine)"
    fi
    printf 'Choose a Tailscale Service name (one DNS label; examples: sam-g-server, sam-g-laptop): ' >&2
    IFS= read -r service_name || fail "could not read a service name"
  fi
  if [[ ! $service_name =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    fail "invalid service name '$service_name': use one lowercase DNS label (1-63 letters, digits, or internal hyphens); dots such as sam-g.laptop are not supported"
  fi
  if [[ $assume_yes != true && ( ! -t 0 || ! -t 1 ) ]]; then
    fail "service setup needs interactive confirmation; use --tailscale-service $service_name --yes explicitly after reviewing the change"
  fi

  configured_listen=$(jq -er '
    if (.server | type) != "object" then error("server.listen must be explicitly configured in a server object")
    elif (.server | has("listen") | not) then error("server.listen must be explicitly configured")
    elif (.server.listen | type) != "string" then error("server.listen must be a string")
    elif .server.listen == "" then error("server.listen must not be empty")
    else .server.listen end
  ' "$config" 2>&1) || fail "could not read server.listen from $config: $configured_listen"
  if [[ ! $configured_listen =~ ^127\.0\.0\.1:([0-9]{1,5})$ ]]; then
    fail "Tailscale Services requires sam-g to listen explicitly on 127.0.0.1:PORT (the supported local proxy target); found '$configured_listen'. Set server.listen to a matching IPv4 loopback address and port, then retry"
  fi
  listen_port=$((10#${BASH_REMATCH[1]}))
  if (( listen_port < 1 || listen_port > 65535 )); then
    fail "server.listen has an invalid TCP port: '$configured_listen'"
  fi
  service_target="http://127.0.0.1:$listen_port"

  ts_version=$(tailscale version 2>&1) || fail "could not read the Tailscale version: $ts_version"
  ts_version=${ts_version%%$'\n'*}
  if [[ ! $ts_version =~ ^([0-9]+)\.([0-9]+)(\.|[-+]|$) ]]; then
    fail "could not understand Tailscale version '$ts_version'; Tailscale 1.86 or newer is required"
  fi
  ts_major=${BASH_REMATCH[1]}
  ts_minor=${BASH_REMATCH[2]}
  if (( ts_major < 1 || (ts_major == 1 && ts_minor < 86) )); then
    fail "Tailscale 1.86 or newer is required for Tailscale Services (found $ts_version)"
  fi

  tailscale_status=$(tailscale status --json 2>&1) || fail "Tailscale is not connected on this machine; sign in/connect this host first. Details: $tailscale_status"
  backend_state=$(jq -r '.BackendState // ""' <<<"$tailscale_status" 2>/dev/null) || fail "could not read Tailscale status JSON"
  [[ $backend_state == Running ]] || fail "Tailscale is not running/connected on this machine (BackendState: ${backend_state:-unknown}); connect it and retry"
  if ! jq -e '(.Self.Tags // []) | (type == "array" and length > 0)' >/dev/null <<<"$tailscale_status" 2>/dev/null; then
    fail "this machine does not report a tag-based Tailscale identity (Self.Tags is empty or missing). Tailscale Services requires a tagged host; ask an admin to decide and apply the appropriate host identity manually. This script will not retag it"
  fi
  if ! jq -e '(.Self.CapMap // {}) as $cap | ($cap | type) == "object" and ($cap | has("https"))' >/dev/null <<<"$tailscale_status" 2>/dev/null; then
    fail "HTTPS support is not already enabled for this tailnet (Self.CapMap lacks https). Ask an admin to enable Tailscale HTTPS in the admin console, then retry; this helper will not enable it or wait for an interactive tailnet change"
  fi

  service_config=$(tailscale serve get-config --all 2>&1) || fail "could not inspect this machine's Tailscale Services config; leaving it unchanged. Details: $service_config"
  service_key="svc:$service_name"
  endpoint_json=$(jq -c --arg service "$service_key" '
    if type != "object" then error("expected a JSON object")
    elif has("services") and (.services | type) != "object" then error("services must be an object")
    else
      (if has("services") and (.services | has($service)) then .services[$service] else {} end) as $svc
      | if ($svc | type) != "object" then error("service entry must be an object")
        elif ($svc | has("endpoints")) and ($svc.endpoints | type) != "object" then error("service endpoints must be an object")
        else
          (if ($svc | has("endpoints")) then $svc.endpoints else {} end) as $endpoints
          | if ($endpoints | has("tcp:443")) then
              $endpoints["tcp:443"] as $endpoint
              | if ($endpoint | type) == "string" then $endpoint else error("tcp:443 endpoint must be a string") end
            else null end
        end
    end
  ' <<<"$service_config" 2>&1) || fail "could not safely inspect the Tailscale Services config; no route was changed: $endpoint_json"

  if [[ $endpoint_json != null ]]; then
    existing_endpoint=$(jq -r '.' <<<"$endpoint_json")
    if [[ $existing_endpoint == "$service_target" ]]; then
      printf 'Tailscale Service %s already maps HTTPS 443 to %s on this machine; leaving that mapping unchanged.\n' "$service_key" "$service_target"
    else
      fail "$service_key already has a different tcp:443 endpoint ('$existing_endpoint'); leaving it and all other routes unchanged. Resolve the mapping manually before retrying"
    fi
  else
    if [[ $assume_yes != true ]]; then
      cat >&2 <<EOF
This will configure ONLY this machine's Tailscale Service endpoint:
  Service:  $service_key
  Endpoint: HTTPS 443 -> $service_target
Use a service name dedicated to this machine/database; do not reuse it on
another independent host. It will not modify ordinary device Serve routes,
another Service/host, sam-g config, Tailscale tags, admin grants, or Funnel. The
endpoint persists if sam-g stops; this does not start sam-g at boot.
Before continuing, make sure an admin has defined this Service specifically for
this sam-g instance in the Tailscale admin console with tcp:443. This name must
not be shared with another machine that has its own database. An admin may still
need to approve this host and separately grant clients access to $service_key on port 443.
Continue and then start this machine's sam-g process? [y/N] 
EOF
      IFS= read -r answer || fail "could not read confirmation"
      case "$answer" in
        y|Y|yes|YES|Yes) ;;
        *) printf 'Cancelled; no Tailscale route or sam-g process was started.\n' >&2; exit 0 ;;
      esac
    fi

    printf 'Configuring only this machine as the host for %s...\n' "$service_key"
    tailscale_output=$(tailscale serve "--service=$service_key" --https=443 "$service_target" 2>&1) || {
      printf '%s\n' "$tailscale_output" >&2
      fail "Tailscale did not configure $service_key. No broad reset or peer changes were attempted; inspect 'tailscale serve get-config --all' before retrying"
    }
    printf 'Configured this machine only: %s HTTPS 443 -> %s\n' "$service_key" "$service_target"
  fi

  tailnet_suffix=$(jq -r '
    (.Self.DNSName // "") as $dns
    | if ($dns | type) != "string" then ""
      else ($dns | rtrimstr(".") | split(".")) as $labels
      | if ($labels | length) >= 3 then ($labels[1:] | join(".")) else "" end
      end
  ' <<<"$tailscale_status" 2>/dev/null) || tailnet_suffix=
  if [[ -n $tailnet_suffix ]]; then
    service_hostname="$service_name.$tailnet_suffix"
    printf 'Service URL after host approval and access grants: https://%s/\n' "$service_hostname"
    printf 'App configuration: add "http://%s" to server.allowed_origins for the forwarded proxy Host.\n' "$service_hostname"
    printf 'Also allow the exact browser frontend origin; keep existing entries. This helper does not edit those settings.\n'
  else
    printf 'Service hostname is not available in local status; check Tailscale Admin Console after host approval.\n'
  fi
  printf 'Admin follow-up: approve this host if required, then grant the intended clients access to %s on port 443.\n' "$service_key"
  printf 'Tailscale Services route configuration is separate from the sam-g process and does not supervise it after reboot.\n\n'
fi

if [[ $background == true ]]; then
  umask 077
  logs_dir=$sam_g_dir/logs
  log_file=$logs_dir/server.log
  mkdir -p "$logs_dir"
  chmod 700 "$sam_g_dir" "$logs_dir"
  touch "$log_file"
  chmod 600 "$log_file"

  nohup "$binary" -config "$config" >>"$log_file" 2>&1 </dev/null &
  pid=$!
  sleep 1
  if ! kill -0 "$pid" 2>/dev/null; then
    wait "$pid" 2>/dev/null || true
    fail "sam-g exited during startup; inspect $log_file"
  fi

  printf 'sam-g started on this machine in the background (PID %s).\n' "$pid"
  printf 'Logs are appended to %s\n' "$log_file"
  printf 'Before stopping it, verify the PID still belongs to sam-g, then run: kill -TERM %s\n' "$pid"
  if [[ -n $service_target ]]; then
    printf 'Readiness target: %s/readyz\n' "$service_target"
  else
    printf 'Check readiness at the server.listen address in %s (default: http://127.0.0.1:8080/readyz).\n' "$config"
  fi
  printf 'Background mode survives closing this terminal, not a reboot; configure systemd/launchd separately if desired.\n'
else
  printf 'Starting the sam-g process on this machine in the foreground. Ctrl+C stops it.\n' >&2
  exec "$binary" -config "$config"
fi
