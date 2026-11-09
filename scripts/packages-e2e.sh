#!/usr/bin/env bash
# End-to-end test of the deb and rpm packages of a snapshot (dist/), in Debian
# and Fedora containers running systemd: install, the service disabled until
# configured, start against a simulator, upgrade, removal keeping the
# configuration and the state store. Needs Docker.
#
# Usage: scripts/packages-e2e.sh [dist]
set -euo pipefail

dist="${1:-dist}"
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

# The checks, run inside the container: INSTALL, UPGRADE, REMOVE and PURGE are
# the commands of the package manager.
checks() {
  cat <<'INNER'
set -uo pipefail
fail() { echo "packages e2e failed: $*" >&2; systemctl status tapyr --no-pager -l >&2 || true; journalctl -u tapyr --no-pager -n 40 >&2 || true; exit 1; }
wait_for() { local what="$1"; shift; for _ in $(seq 1 60); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done; fail "$what"; }
ready() { curl -sf 127.0.0.1:9790/readyz | grep -q '"ready":true'; }
metric() { curl -sf 127.0.0.1:9790/metrics | grep -q '^example_device_up{'; }

$INSTALL || fail "install"
tapyr version | head -1
test -f /usr/lib/systemd/system/tapyr.service || fail "no unit"
test -f /etc/tapyr/config.yaml.example || fail "no example"
[ "$(systemctl is-enabled tapyr)" = disabled ] || fail "the service must not be enabled by the install"
if systemctl is-active -q tapyr; then fail "the service must not start by itself"; fi

nohup tapyr sim --provider example-sim >/tmp/sim.log 2>&1 &
cat >/etc/tapyr/config.yaml <<YAML
apiVersion: tapyr/v1alpha1
kind: Settings
metadata: { name: e2e }
spec: { collection: { jitter: "0%" } }
---
apiVersion: tapyr/v1alpha1
kind: Account
metadata: { name: demo }
spec:
  provider: example-sim
  params: { baseURL: "http://127.0.0.1:8081/api/v1" }
  secrets: { apiKey: { env: TAPYR_EXAMPLE_API_KEY } }
---
apiVersion: tapyr/v1alpha1
kind: Job
metadata: { name: devices }
spec: { account: demo, template: example-sim-devices, interval: 1m }
---
apiVersion: tapyr/v1alpha1
kind: Output
metadata: { name: prom }
spec: { type: prometheus }
YAML
install -m 0600 /dev/null /etc/tapyr/tapyr.env
echo TAPYR_EXAMPLE_API_KEY=sim-api-key >/etc/tapyr/tapyr.env
systemctl enable --now tapyr || fail "enable"
wait_for "the service did not become ready" ready
wait_for "the metrics did not arrive" metric
echo "started: ready, metrics collected"

before="$(systemctl show -p MainPID --value tapyr)"
sleep 6
$UPGRADE || fail "upgrade"
wait_for "the service did not restart after the upgrade" ready
after="$(systemctl show -p MainPID --value tapyr)"
[ "$before" != "$after" ] || fail "the upgrade did not restart the service"
journalctl -u tapyr --no-pager | grep -q '"ledger_entries":[1-9]' || fail "the state store was not restored"
echo "upgraded: restarted, state restored"

$REMOVE || fail "remove"
if systemctl is-active -q tapyr; then fail "the service still runs after the removal"; fi
test ! -e /usr/bin/tapyr || fail "the binary is still there"
test -f /etc/tapyr/config.yaml || fail "the removal deleted the configuration"
test -f /var/lib/private/tapyr/data/tapyr.db || fail "the removal deleted the state store"
$PURGE || fail "purge"
test -f /var/lib/private/tapyr/data/tapyr.db || fail "the purge deleted the state store"
echo "removed: configuration and state store kept"
INNER
}

# run_distro <name> <image setup> <package> <install> <upgrade> <remove> <purge>
run_distro() {
  local name="$1" dockerfile="$2" pkg="$3" install="$4" upgrade="$5" remove="$6" purge="$7"
  local c="tapyr-pkg-$name"
  [[ -f "$pkg" ]] || { echo "no package $pkg" >&2; exit 1; }
  echo "== $name: $(basename "$pkg")"
  docker build -q -t "$c" - <<<"$dockerfile" >/dev/null
  docker rm -f "$c" >/dev/null 2>&1 || true
  docker run -d --name "$c" --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    --tmpfs /run --tmpfs /run/lock "$c" >/dev/null
  trap 'docker rm -f "$c" >/dev/null 2>&1 || true' RETURN
  docker exec "$c" timeout 60 systemctl is-system-running --wait >/dev/null 2>&1 || true
  docker cp "$pkg" "$c:/root/$(basename "$pkg")"
  local p="/root/$(basename "$pkg")"
  checks | docker exec -i -e INSTALL="$install $p" -e UPGRADE="$upgrade $p" -e REMOVE="$remove" -e PURGE="$purge" "$c" bash -s
}

run_distro debian "FROM debian:13-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends systemd systemd-sysv curl ca-certificates procps
CMD [\"/sbin/init\"]" \
  "$(ls "$dist"/tapyr_*_linux_"$arch".deb)" "dpkg -i" "dpkg -i" "dpkg -r tapyr" "dpkg -P tapyr"

run_distro fedora "FROM fedora:43
RUN dnf install -y -q systemd curl procps-ng
CMD [\"/usr/sbin/init\"]" \
  "$(ls "$dist"/tapyr_*_linux_"$arch".rpm)" "rpm -i" "rpm -U --replacepkgs" "rpm -e tapyr" "true"

echo "packages e2e passed"
