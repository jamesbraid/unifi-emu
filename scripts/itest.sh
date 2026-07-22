#!/usr/bin/env bash
# itest.sh — live adoption proof against a real controller.
#
# Recreates unifi-itest-ctrl fresh (demo devices re-seed PENDING), informs
# emulated device(s) from the host, adopts them, and asserts the controller
# reports state=1 adopted=true for each. All evidence lands in tmp/itest/.
#
# Requires: docker, go, jq, curl (go only for host mode).
# Usage: itest.sh [MAC] [MODEL]           — one host-run device (default UGW3 00:27:22:e0:00:01)
#        itest.sh fleet                   — every device in scripts/devices.fleet.json, host-run
#        itest.sh docker [--build] [MAC] [MODEL] — sim runs as container unifi-emu-smoke
#        itest.sh docker [--build] fleet — same, fleet mounted into the container
#
# Topology: macOS cannot route to container IPs, so for a HOST-run sim the
# controller is started with SYSTEM_IP=127.0.0.1 (entrypoint writes
# system_ip into system.properties), making the post-adopt inform uri
# http://127.0.0.1:8080/inform — reachable by a sim running on the host.
# (The REST mgmt doc on this build has no inform-host knob; inform_host,
# x_inform_host and override_inform_host are all stripped on write.
# localhost itself is rejected: the controller validates the payload's
# inform_ip as an IP literal — "invalid inform_ip localhost" in
# server.log — so everything uses 127.0.0.1.)
#
# docker mode: the sim runs as a container on the same network, so the
# override would break it (127.0.0.1 inside the sim container is itself).
# The controller boots WITHOUT SYSTEM_IP and advertises its own address
# http://172.30.0.2:8080/inform — an IP literal (passes inform_ip
# validation) reachable from any container on unifi-itest.
set -euo pipefail
cd "$(dirname "$0")/.."

CTRL=unifi-itest-ctrl
SIM_CTR=unifi-emu-smoke
NET=unifi-itest
CTRL_IP=172.30.0.2
API=https://localhost:8443
INFORM=http://127.0.0.1:8080/inform
IMG=ghcr.io/jamesbraid/unifi-network:sim
EMU_IMG=unifi-emu:dev
OUT=tmp/itest
SIM_PID=""
DOCKER=""
BUILD=""
FLEET=""
SYSIP="-e SYSTEM_IP=127.0.0.1"
mkdir -p "$OUT"

if [ "${1:-}" = docker ]; then
	DOCKER=1
	shift
	if [ "${1:-}" = --build ]; then BUILD=1; shift; fi
	INFORM="http://$CTRL_IP:8080/inform"
	SYSIP=""
fi

if [ "${1:-}" = fleet ]; then
	FLEET=scripts/devices.fleet.json
	MACS=()
	while IFS= read -r m; do MACS+=("$m"); done < <(jq -r '.[].mac' "$FLEET")
	[ ${#MACS[@]} -gt 0 ] || { echo "no devices in $FLEET" >&2; exit 1; }
	SIM_ARGS=(-devices "$FLEET")
	# scratch has no repo checkout; the fleet file is mounted at /devices.json
	if [ -n "$DOCKER" ]; then SIM_ARGS=(-devices /devices.json); fi
else
	if [ -n "$DOCKER" ]; then
		MAC="${1:-00:27:22:e0:00:31}"
		MODEL="${2:-UGW3}"
		SIM_ARGS=(-mac "$MAC" -model "$MODEL" -ip 172.30.0.31)
	else
		MAC="${1:-00:27:22:e0:00:01}"
		MODEL="${2:-UGW3}"
		SIM_ARGS=(-mac "$MAC" -model "$MODEL")
	fi
	MACS=("$MAC")
fi

log() { printf '\n==> %s\n' "$*"; }

macf() { tr -d ':' <<<"$1"; } # filename-safe form of a MAC

capture() { # best-effort evidence capture, safe to run any time
	docker logs "$CTRL" >"$OUT/controller.log" 2>&1 || true
	docker exec "$CTRL" tail -200 /usr/lib/unifi/logs/server.log >"$OUT/server.log" 2>&1 || true
	[ -z "$DOCKER" ] || docker logs "$SIM_CTR" >"$OUT/sim.log" 2>&1 || true
}

sim_alive() {
	if [ -n "$DOCKER" ]; then
		[ "$(docker inspect -f '{{.State.Running}}' "$SIM_CTR" 2>/dev/null)" = true ]
	else
		kill -0 "$SIM_PID" 2>/dev/null
	fi
}

cleanup() {
	capture
	[ -n "$SIM_PID" ] && kill "$SIM_PID" 2>/dev/null || true
	[ -z "$DOCKER" ] || docker rm -f "$SIM_CTR" >/dev/null 2>&1 || true
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

device_doc() { # the stat/device doc for $1 (a MAC), empty when absent
	api GET /api/s/default/stat/device |
		jq --arg mac "$1" '[.data[] | select(.mac | ascii_downcase == ($mac | ascii_downcase))] | .[0] // empty'
}

if [ -n "$DOCKER" ]; then
	log "1/9 recreate controller $CTRL (fresh, no SYSTEM_IP override)"
else
	log "1/9 recreate controller $CTRL (fresh, SYSTEM_IP=127.0.0.1)"
fi
docker rm -f "$CTRL" >/dev/null 2>&1 || true
docker network inspect "$NET" >/dev/null 2>&1 || docker network create --subnet 172.30.0.0/24 "$NET" >/dev/null
docker run -d --name "$CTRL" --network "$NET" --ip "$CTRL_IP" \
	$SYSIP -p 8443:8443 -p 8080:8080 "$IMG" >/dev/null
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

if [ -n "$DOCKER" ]; then
	# No override: the controller must advertise its own address, which the
	# sim container reaches directly over $NET.
	log "3/9 verify adopt_url (must be $INFORM)"
	for _ in $(seq 1 30); do
		urls=$(api GET /api/s/default/stat/device | jq -r '.data[] | select(.adopted==false) | .adopt_url' | sort -u)
		[ -n "$urls" ] && grep -qx "$INFORM" <<<"$urls" && break
		sleep 2
	done
	grep -qx "$INFORM" <<<"$urls" ||
		fail "adopt_url not $INFORM, pending devices advertise: ${urls:-none}"
else
	log "3/9 verify inform-host override (adopt_url must be $INFORM, not $CTRL_IP)"
	for _ in $(seq 1 30); do
		urls=$(api GET /api/s/default/stat/device | jq -r '.data[] | select(.adopted==false) | .adopt_url' | sort -u)
		[ -n "$urls" ] && ! grep -q "$CTRL_IP" <<<"$urls" && break
		sleep 2
	done
	grep -qx "$INFORM" <<<"$urls" ||
		fail "adopt_url override not in effect, pending devices advertise: ${urls:-none}"
fi

if [ -n "$DOCKER" ]; then
	log "4/9 start sim container $SIM_CTR (${#MACS[@]} device(s): ${MACS[*]})"
	if [ -n "$BUILD" ]; then
		docker build -t "$EMU_IMG" .
	elif ! docker image inspect "$EMU_IMG" >/dev/null 2>&1; then
		fail "image $EMU_IMG missing; run '$0 docker --build' or: docker build -t $EMU_IMG ."
	fi
	docker rm -f "$SIM_CTR" >/dev/null 2>&1 || true
	MOUNT=""
	if [ -n "$FLEET" ]; then MOUNT="-v $PWD/$FLEET:/devices.json:ro"; fi
	docker run -d --name "$SIM_CTR" --network "$NET" $MOUNT \
		"$EMU_IMG" -inform "$INFORM" "${SIM_ARGS[@]}" >/dev/null
else
	log "4/9 build + start sim (${#MACS[@]} device(s): ${MACS[*]})"
	go build -o "$OUT/unifi-emu" ./cmd/unifi-emu
	"$OUT/unifi-emu" -inform "$INFORM" "${SIM_ARGS[@]}" >"$OUT/sim.log" 2>&1 &
	SIM_PID=$!
fi

log "5/9 wait for all ${#MACS[@]} device(s) pending (state=2)"
deadline=$((SECONDS + 120))
waiting="x"
while [ $SECONDS -lt $deadline ]; do
	waiting=""
	for mac in "${MACS[@]}"; do
		doc=$(device_doc "$mac")
		if [ -n "$doc" ] && [ "$(jq -r .state <<<"$doc")" = 2 ]; then
			echo "$doc" | jq . >"$OUT/device-pending-$(macf "$mac").json"
		else
			waiting="$waiting $mac"
		fi
	done
	[ -z "$waiting" ] && break
	sim_alive || fail "sim died; see $OUT/sim.log"
	sleep 2
done
[ -z "$waiting" ] || fail "never pending:$waiting"

# Adopts are serialized, one device fully connected before the next, and
# rejections are retried: this controller build answers devmgr adopt with
# api.err.CannotAdopt / api.err.CanNotAdoptUnknownDevice when the device
# doc is too fresh (a pending doc sighted 2s after the first inform is
# not adoptable yet; minutes later the same adopt succeeds) and a failed
# adopt can reap the doc entirely, in which case the sim's next inform
# re-creates it. A human clicks Adopt once per device and re-clicks on
# failure; do the same for about two minutes.
log "6/9 adopt each of ${#MACS[@]} device(s), waiting for state=1 adopted=true"
for mac in "${MACS[@]}"; do
	adopted=""
	for _ in $(seq 1 12); do
		resp=$(api POST /api/s/default/cmd/devmgr "{\"cmd\":\"adopt\",\"mac\":\"$mac\"}")
		if jq -e '.meta.rc=="ok"' <<<"$resp" >/dev/null; then
			adopted=1
			break
		fi
		grep -qi 'cannotadopt' <<<"$resp" || fail "adopt $mac rejected: $resp"
		# CanNotAdoptUnknownDevice means the doc was reaped; the sim's
		# next inform re-creates it. CannotAdopt can also mean "already
		# adopting" (an earlier attempt landed controller-side); the doc
		# is the source of truth.
		doc=$(device_doc "$mac")
		if [ -n "$doc" ] && [ "$(jq -r .adopted <<<"$doc")" = true ]; then
			adopted=1
			break
		fi
		sleep 10
	done
	[ -n "$adopted" ] || fail "adopt $mac rejected with CannotAdopt 12 times: $resp"
	echo "$resp" >"$OUT/adopt-$(macf "$mac").json"
	deadline=$((SECONDS + 90))
	final=""
	while [ $SECONDS -lt $deadline ]; do
		final=$(device_doc "$mac")
		[ -n "$final" ] && [ "$(jq -r .state <<<"$final")" = 1 ] && [ "$(jq -r .adopted <<<"$final")" = true ] && break
		sleep 2
	done
	[ -n "$final" ] && [ "$(jq -r .state <<<"$final")" = 1 ] && [ "$(jq -r .adopted <<<"$final")" = true ] ||
		fail "$mac never connected; last doc: ${final:-absent}"
	echo "$final" | jq . >"$OUT/device-final-$(macf "$mac").json"
	echo "    $mac connected ($(jq -r .model <<<"$final"))"
done

log "7/9 all ${#MACS[@]} device(s) connected"
if [ ${#MACS[@]} -eq 1 ]; then
	cp "$OUT/device-final-$(macf "${MACS[0]}").json" "$OUT/device-final.json"
else
	files=()
	for mac in "${MACS[@]}"; do files+=("$OUT/device-final-$(macf "$mac").json"); done
	jq -s '.' "${files[@]}" >"$OUT/device-final.json"
fi

log "8/9 capture evidence"
capture
# Count only watch()'s "ADOPTING -> CONNECTED" transitions: plain
# '-> CONNECTED' also matches loop.go's "adoption handshake complete ->
# CONNECTED" (2 lines per device), which would let the gate pass with
# half the fleet still adopting.
for _ in $(seq 1 10); do
	# docker mode: sim.log is a snapshot, refresh it while waiting
	[ -z "$DOCKER" ] || docker logs "$SIM_CTR" >"$OUT/sim.log" 2>&1 || true
	[ "$(grep -c -- 'ADOPTING -> CONNECTED' "$OUT/sim.log" || true)" -ge "${#MACS[@]}" ] && break
	sleep 1.5
done
[ "$(grep -c -- 'ADOPTING -> CONNECTED' "$OUT/sim.log" || true)" -ge "${#MACS[@]}" ] ||
	fail "controller adopted but sim never reached CONNECTED for all; see $OUT/sim.log"

log "9/9 result"
for mac in "${MACS[@]}"; do
	jq -r '"\(.mac) state=\(.state) adopted=\(.adopted) model=\(.model) ip=\(.ip) version=\(.version)"' \
		"$OUT/device-final-$(macf "$mac").json"
done
grep -m 20 -E 'inform: HTTP (404|200)|set-adopt|-> (ADOPTING|CONNECTED)' "$OUT/sim.log"
echo "CONNECTED ✔ (${#MACS[@]} device(s), evidence in $OUT/)"
