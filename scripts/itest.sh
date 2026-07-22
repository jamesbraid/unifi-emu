#!/usr/bin/env bash
# itest.sh — live adoption proof against a real controller.
#
# Recreates unifi-itest-ctrl fresh (demo devices re-seed PENDING), informs
# one emulated device from the host, adopts it, and asserts the controller
# reports state=1 adopted=true. All evidence lands in tmp/itest/.
#
# Requires: docker, go, jq, curl. Usage: itest.sh [MAC] [MODEL]
#
# Topology: macOS cannot route to container IPs, so the controller is
# started with SYSTEM_IP=127.0.0.1 (entrypoint writes system_ip into
# system.properties), making the post-adopt inform uri
# http://127.0.0.1:8080/inform — reachable by a sim running on the host.
# (The REST mgmt doc on this build has no inform-host knob; inform_host,
# x_inform_host and override_inform_host are all stripped on write.
# localhost itself is rejected: the controller validates the payload's
# inform_ip as an IP literal — "invalid inform_ip localhost" in
# server.log — so everything uses 127.0.0.1.)
set -euo pipefail
cd "$(dirname "$0")/.."

MAC="${1:-00:27:22:e0:00:01}"
MODEL="${2:-UGW3}"
CTRL=unifi-itest-ctrl
NET=unifi-itest
CTRL_IP=172.30.0.2
API=https://localhost:8443
INFORM=http://127.0.0.1:8080/inform
IMG=ghcr.io/jamesbraid/unifi-network:sim
OUT=tmp/itest
SIM_PID=""
mkdir -p "$OUT"

log() { printf '\n==> %s\n' "$*"; }

capture() { # best-effort evidence capture, safe to run any time
	docker logs "$CTRL" >"$OUT/controller.log" 2>&1 || true
	docker exec "$CTRL" tail -200 /usr/lib/unifi/logs/server.log >"$OUT/server.log" 2>&1 || true
}

cleanup() {
	capture
	[ -n "$SIM_PID" ] && kill "$SIM_PID" 2>/dev/null || true
}

fail() {
	echo "FAIL: $*" >&2
	capture
	echo "--- $OUT/sim.log (tail) ---" >&2
	tail -30 "$OUT/sim.log" >&2 2>/dev/null || true
	echo "--- $OUT/server.log (tail) ---" >&2
	tail -30 "$OUT/server.log" >&2 2>/dev/null || true
	exit 1
}
trap cleanup EXIT

api() { # api METHOD PATH [BODY] — authenticated curl, response on stdout
	local method="$1" path="$2" body="${3:-}"
	if [ -n "$body" ]; then
		curl -ks -b "$OUT/cookies" -X "$method" "$API$path" \
			-H 'Content-Type: application/json' -d "$body"
	else
		curl -ks -b "$OUT/cookies" -X "$method" "$API$path"
	fi
}

device_doc() { # the stat/device doc for $MAC, empty when absent
	api GET /api/s/default/stat/device |
		jq --arg mac "$MAC" '[.data[] | select(.mac | ascii_downcase == ($mac | ascii_downcase))] | .[0] // empty'
}

log "1/9 recreate controller $CTRL (fresh, SYSTEM_IP=127.0.0.1)"
docker rm -f "$CTRL" >/dev/null 2>&1 || true
docker network inspect "$NET" >/dev/null 2>&1 || docker network create --subnet 172.30.0.0/24 "$NET" >/dev/null
docker run -d --name "$CTRL" --network "$NET" --ip "$CTRL_IP" \
	-e SYSTEM_IP=127.0.0.1 -p 8443:8443 -p 8080:8080 "$IMG" >/dev/null
healthy=""
for _ in $(seq 1 60); do
	healthy=$(docker inspect -f '{{.State.Health.Status}}' "$CTRL" 2>/dev/null || true)
	[ "$healthy" = healthy ] && break
	sleep 5
done
[ "$healthy" = healthy ] || fail "controller not healthy after 5min (last: $healthy)"

log "2/9 login"
for _ in $(seq 1 15); do
	if curl -ks -c "$OUT/cookies" -X POST "$API/api/login" \
		-H 'Content-Type: application/json' \
		-d '{"username":"admin","password":"admin"}' | jq -e '.meta.rc=="ok"' >/dev/null 2>&1; then
		break
	fi
	sleep 2
done
curl -ks -b "$OUT/cookies" "$API/api/s/default/stat/device" | jq -e '.meta.rc=="ok"' >/dev/null ||
	fail "login/session not working"

log "3/9 verify inform-host override (adopt_url must be $INFORM, not $CTRL_IP)"
for _ in $(seq 1 30); do
	urls=$(api GET /api/s/default/stat/device | jq -r '.data[] | select(.adopted==false) | .adopt_url' | sort -u)
	[ -n "$urls" ] && ! grep -q "$CTRL_IP" <<<"$urls" && break
	sleep 2
done
grep -qx "$INFORM" <<<"$urls" ||
	fail "adopt_url override not in effect, pending devices advertise: ${urls:-none}"

log "4/9 build + start sim (mac=$MAC model=$MODEL)"
go build -o "$OUT/unifi-emu" ./cmd/unifi-emu
"$OUT/unifi-emu" -inform "$INFORM" -mac "$MAC" -model "$MODEL" >"$OUT/sim.log" 2>&1 &
SIM_PID=$!

log "5/9 wait for $MAC to appear pending (state=2)"
doc=""
for _ in $(seq 1 60); do
	doc=$(device_doc)
	[ -n "$doc" ] && [ "$(jq -r .state <<<"$doc")" = 2 ] && break
	kill -0 "$SIM_PID" 2>/dev/null || fail "sim died; see $OUT/sim.log"
	sleep 2
done
[ -n "$doc" ] && [ "$(jq -r .state <<<"$doc")" = 2 ] || fail "device never appeared pending; last doc: ${doc:-absent}"
echo "$doc" | jq . >"$OUT/device-pending.json"

log "6/9 adopt $MAC"
api POST /api/s/default/cmd/devmgr "{\"cmd\":\"adopt\",\"mac\":\"$MAC\"}" | tee "$OUT/adopt.json" | jq -e '.meta.rc=="ok"' >/dev/null ||
	fail "adopt rejected: $(cat "$OUT/adopt.json")"

log "7/9 wait for state=1 adopted=true (up to 3min)"
final=""
for _ in $(seq 1 90); do
	final=$(device_doc)
	if [ -n "$final" ] && [ "$(jq -r .state <<<"$final")" = 1 ] && [ "$(jq -r .adopted <<<"$final")" = true ]; then
		break
	fi
	sleep 2
done
[ -n "$final" ] && [ "$(jq -r .state <<<"$final")" = 1 ] && [ "$(jq -r .adopted <<<"$final")" = true ] ||
	fail "device never connected; last doc: ${final:-absent}"
echo "$final" | jq . >"$OUT/device-final.json"

log "8/9 capture evidence"
capture
for _ in $(seq 1 10); do
	grep -q CONNECTED "$OUT/sim.log" && break
	sleep 1.5
done
grep -q CONNECTED "$OUT/sim.log" || fail "controller adopted but sim never reached CONNECTED; see $OUT/sim.log"

log "9/9 result"
jq -r '"state=\(.state) adopted=\(.adopted) model=\(.model) ip=\(.ip) version=\(.version)"' "$OUT/device-final.json"
grep -m 10 -E 'inform: HTTP (404|200)|set-adopt|-> (ADOPTING|CONNECTED)' "$OUT/sim.log"
echo "CONNECTED ✔ (evidence in $OUT/)"
