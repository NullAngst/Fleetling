#!/usr/bin/env bash
# Runs the browser smoke test (smoke.mjs) against a fresh Fleetling: a
# temporary stack root and data folder, a throwaway container to open a
# shell in, and the server on loopback with TLS off.
#
# Usage: bash smoke.sh /path/to/fleetling
#
# Needs docker with a running daemon, node with this folder's packages
# installed (npm ci --ignore-scripts), and Chrome or Chromium. CHROME_PATH
# picks the browser, SMOKE_IMAGE the container image (alpine:latest) and
# SMOKE_PORT the port (8420).
set -euo pipefail

bin=${1:-}
if [ -z "$bin" ] || [ ! -x "$bin" ]; then
	printf 'usage: bash smoke.sh /path/to/fleetling\n' >&2
	exit 2
fi

missing=()
for c in docker node curl grep cut head mktemp; do
	command -v "$c" >/dev/null 2>&1 || missing+=("$c")
done
chrome=${CHROME_PATH:-}
if [ -z "$chrome" ]; then
	for c in google-chrome google-chrome-stable chromium chromium-browser; do
		if chrome=$(command -v "$c"); then
			break
		fi
		chrome=
	done
fi
[ -n "$chrome" ] || missing+=("google-chrome or chromium")
if [ "${#missing[@]}" -gt 0 ]; then
	printf 'smoke.sh: missing: %s\n' "${missing[*]}" >&2
	exit 1
fi

here=$(cd -- "$(dirname -- "$0")" && pwd -P)
image=${SMOKE_IMAGE:-alpine:latest}
port=${SMOKE_PORT:-8420}
work=$(mktemp -d "${TMPDIR:-/tmp}/fleetling-smoke.XXXXXX")
ctr=fleetling-smoke-$$
pid=

# On exit, whatever happened: stop the server, drop the container and the
# temporary folders.
trap 'if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi; docker rm -f "$ctr" >/dev/null 2>&1 || true; rm -rf -- "$work"' EXIT

mkdir -p -- "$work/stacks/smoke"
printf 'services:\n  app:\n    image: %s\n    command: sleep 600\n' "$image" >"$work/stacks/smoke/compose.yaml"
docker run -d --name "$ctr" "$image" sleep 600 >/dev/null
id=$(docker inspect -f '{{.Id}}' "$ctr")

FLEETLING_TLS=off FLEETLING_ADDR="127.0.0.1:$port" \
	FLEETLING_ROOT="$work/stacks" FLEETLING_DATA="$work/data" \
	"$bin" >"$work/fleetling.log" 2>&1 &
pid=$!

# The setup token is in the first-run log line, once the server answers.
token=
i=0
while [ "$i" -lt 30 ]; do
	if curl -fs "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
		token=$(LC_ALL=C grep -o 'token=[0-9a-f]*' "$work/fleetling.log" | head -n 1 | cut -d= -f2)
		break
	fi
	sleep 1
	i=$((i + 1))
done
if [ -z "$token" ]; then
	printf 'smoke.sh: Fleetling did not start, or printed no setup token:\n' >&2
	cat -- "$work/fleetling.log" >&2
	exit 1
fi

status=0
FLEETLING_URL="http://127.0.0.1:$port" FLEETLING_SETUP_TOKEN="$token" \
	SMOKE_STACK=smoke SMOKE_CONTAINER="$id" CHROME_PATH="$chrome" \
	node "$here/smoke.mjs" || status=$?
if [ "$status" -ne 0 ]; then
	printf '\nsmoke.sh: server log:\n' >&2
	cat -- "$work/fleetling.log" >&2
fi
exit "$status"
