#!/usr/bin/env bash
# Open the ClusterView UI: port-forward the UI Service and open a browser.
# Uses a random free port when 8080 is taken, and reconnects if the forward
# drops. Ctrl-C stops it.
#
# Usage: make ui [PORT=8080] [NO_BROWSER=1]
set -euo pipefail

NS=clusterview-operator-system
SVC=clusterview-operator-ui
DEPLOY=clusterview-operator-controller-manager
PORT="${PORT:-8080}"
: "${KUBECTL:=kubectl}"

cd "$(dirname "$0")/.."
# shellcheck source=hack/env.sh
source hack/env.sh

say() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null || die "curl not found on PATH"
"$KUBECTL" config current-context >/dev/null 2>&1 ||
  die "kubectl has no current context. Fill in KUBECONFIG in .env (see .env.example)"
[[ "$("$KUBECTL" auth can-i create pods/portforward -n "$NS" 2>/dev/null)" == yes ]] ||
  die "your user can't port-forward in $NS (needs get services, get pods, create pods/portforward)"
"$KUBECTL" -n "$NS" rollout status "deploy/$DEPLOY" --timeout=120s >/dev/null ||
  die "the operator isn't ready. Run 'make up', or check: kubectl -n $NS get pods"

is_clusterview() { curl -sf --max-time 2 "http://localhost:$1/api/v1/summary" 2>/dev/null | grep -q nodeCount; }
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
open_browser() {
  [[ "${NO_BROWSER:-}" == 1 ]] && return 0
  if command -v open >/dev/null; then open "$1"
  elif command -v xdg-open >/dev/null; then xdg-open "$1" >/dev/null 2>&1 || true
  fi
}

# Pick the port once (not via `port-forward :80`) so a reconnect keeps the
# same URL and the open page recovers.
port="$PORT"
if port_busy "$port"; then
  for _ in $(seq 1 50); do
    port=$((49152 + RANDOM % 16384))   # IANA dynamic range
    port_busy "$port" || break
  done
  port_busy "$port" && die "couldn't find a free local port"
  say "port $PORT is in use, using $port"
fi
url="http://localhost:$port"

pf_pid=""
trap '[[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null || true' EXIT
trap 'exit 0' INT TERM

# port-forward exits when the operator pod restarts (e.g. after a redeploy).
# Restarting it here lets the open page recover without rerunning anything.
opened=""
while true; do
  "$KUBECTL" -n "$NS" port-forward "svc/$SVC" "$port:80" >/dev/null 2>&1 &
  pf_pid=$!
  for _ in $(seq 1 40); do
    is_clusterview "$port" && break
    kill -0 "$pf_pid" 2>/dev/null || break
    sleep 0.25
  done
  if [[ -z "$opened" ]]; then
    is_clusterview "$port" ||
      die "port-forward didn't come up. Try: $KUBECTL -n $NS port-forward svc/$SVC $port:80"
    say "ClusterView UI: $url  (Ctrl-C to stop)"
    open_browser "$url"
    opened=1
  fi
  wait "$pf_pid" || true
  say "port-forward dropped, reconnecting"
  sleep 1
done
