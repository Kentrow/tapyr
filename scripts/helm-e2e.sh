#!/usr/bin/env bash
# End-to-end test of the Helm chart on a kind cluster, with the image built
# from this tree and the example-sim simulator: the pod becomes ready, the
# metrics arrive, a change of the configuration rolls it out, and the state
# store survives the restart. Needs Docker, helm and kubectl.
#
# Usage: scripts/helm-e2e.sh
set -euo pipefail

KIND_VERSION="${KIND_VERSION:-v0.33.0}"
cluster=tapyr-helm-e2e
image=tapyr:helm-e2e
ns=tapyr-e2e
port=19791
tmp="$(mktemp -d)"
pf_pid=""

kind() { go run "sigs.k8s.io/kind@${KIND_VERSION}" "$@"; }

stop_forward() {
  [[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null || true
  pf_pid=""
}

cleanup() {
  stop_forward
  kind delete cluster --name "$cluster" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

fail() {
  echo "helm e2e failed: $*" >&2
  kubectl -n "$ns" get pods -o wide >&2 || true
  kubectl -n "$ns" logs deploy/tapyr --tail=40 >&2 || true
  exit 1
}

forward() {
  stop_forward
  kubectl -n "$ns" port-forward svc/tapyr "$port:9790" >"$tmp/forward.log" 2>&1 &
  pf_pid=$!
}

# wait_for <description> <command...>: retries for 2 minutes.
wait_for() {
  local what="$1"
  shift
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  fail "$what"
}

has_metric() { curl -sf "127.0.0.1:$port/metrics" | grep -q '^example_device_up{'; }
is_ready() { curl -sf "127.0.0.1:$port/readyz" | grep -q '"ready":true'; }

write_values() {
  cat >"$tmp/values.yaml" <<YAML
image: { repository: tapyr, tag: helm-e2e, pullPolicy: Never }
existingSecret: tapyr-e2e
settings:
  collection: { jitter: "0%" }
config: |
  apiVersion: tapyr/v1alpha1
  kind: Account
  metadata: { name: demo }
  spec:
    provider: example-sim
    params: { baseURL: "http://sim:8081/api/v1" }
    secrets: { apiKey: { env: TAPYR_EXAMPLE_API_KEY } }
  ---
  apiVersion: tapyr/v1alpha1
  kind: Job
  metadata: { name: devices }
  spec: { account: demo, template: example-sim-devices, interval: $1 }
  ---
  apiVersion: tapyr/v1alpha1
  kind: Output
  metadata: { name: prom }
  spec: { type: prometheus }
YAML
}

docker build -q -t "$image" .
kind create cluster --name "$cluster" --wait 120s
kind load docker-image "$image" --name "$cluster"

kubectl create namespace "$ns"
kubectl -n "$ns" create secret generic tapyr-e2e --from-literal=TAPYR_EXAMPLE_API_KEY=sim-api-key
kubectl -n "$ns" run sim --image="$image" --image-pull-policy=Never --port=8081 --expose \
  -- sim --provider example-sim --listen 0.0.0.0:8081
kubectl -n "$ns" wait --for=condition=Ready pod/sim --timeout=120s

write_values 1m
helm install tapyr charts/tapyr --namespace "$ns" --values "$tmp/values.yaml" --wait --timeout 3m \
  || fail "helm install"
forward
wait_for "the pod did not become ready" is_ready
wait_for "the metrics did not arrive" has_metric
echo "installed: ready, metrics collected"

first="$(kubectl -n "$ns" get pods -l app.kubernetes.io/name=tapyr -o jsonpath='{.items[0].metadata.name}')"
write_values 2m
stop_forward
helm upgrade tapyr charts/tapyr --namespace "$ns" --values "$tmp/values.yaml" --wait --timeout 3m \
  || fail "helm upgrade"
second="$(kubectl -n "$ns" get pods -l app.kubernetes.io/name=tapyr --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')"
[[ "$first" != "$second" ]] || fail "a change of the configuration did not restart the pod"
echo "upgraded: the configuration change rolled the pod out"

forward
wait_for "the pod did not become ready after the upgrade" is_ready
logs="$(kubectl -n "$ns" logs deploy/tapyr)"
grep -q '"msg":"collector started".*"ledger_entries":[1-9]' <<<"$logs" \
  || fail "the state store did not survive the restart"
echo "restarted: the ledger was restored from the volume"

helm uninstall tapyr --namespace "$ns" --wait --timeout 2m || fail "helm uninstall"
echo "helm e2e passed"
