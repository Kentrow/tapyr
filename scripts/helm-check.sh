#!/usr/bin/env bash
# Static checks of the Helm chart: helm lint, the manifests against the
# Kubernetes schemas (kubeconform), and what the templates must guarantee.
#
# Usage: scripts/helm-check.sh
set -euo pipefail

chart=charts/tapyr
KUBECONFORM_VERSION="${KUBECONFORM_VERSION:-v0.8.0}"

fail() {
  echo "helm check failed: $*" >&2
  exit 1
}

render() {
  helm template demo "$chart" --namespace tapyr "$@"
}

helm lint --strict "$chart"

all=(--set ingress.enabled=true --set ingress.host=tapyr.example.com --set serviceMonitor.enabled=true)
render "${all[@]}" | go run "github.com/yannh/kubeconform/cmd/kubeconform@${KUBECONFORM_VERSION}" -strict -summary -ignore-missing-schemas

out="$(render)"
grep -q 'replicas: 1$' <<<"$out" || fail "the Deployment must have one replica"
grep -q 'type: Recreate' <<<"$out" || fail "the Deployment must use Recreate"
grep -q 'kind: PersistentVolumeClaim' <<<"$out" || fail "persistence must be on by default"
if grep -q 'kind: Ingress' <<<"$out"; then fail "the Ingress must be off by default"; fi
if grep -q 'envFrom' <<<"$out"; then fail "no envFrom without existingSecret"; fi

# The chart owns listen and state.path; the rest of settings is merged.
out="$(render --set settings.server.listen=127.0.0.1:1 --set settings.state.path=/nope \
  --set settings.server.basicAuth.username.env=U --set settings.log.level=debug)"
grep -q 'listen: 0.0.0.0:9790' <<<"$out" || fail "the chart must set server.listen"
if grep -q '127.0.0.1:1' <<<"$out"; then fail "settings must not override server.listen"; fi
grep -q 'path: /home/nonroot/data/tapyr.db' <<<"$out" || fail "the chart must set state.path"
grep -q 'env: U' <<<"$out" || fail "settings must be merged"
grep -q 'level: debug' <<<"$out" || fail "settings must be merged"

# Without persistence, the state store is on an emptyDir and no claim exists.
out="$(render --set persistence.enabled=false)"
if grep -q 'kind: PersistentVolumeClaim' <<<"$out"; then fail "no claim without persistence"; fi
if grep -q 'persistentVolumeClaim' <<<"$out"; then fail "no claim volume without persistence"; fi
out="$(render --set persistence.existingClaim=mine)"
if grep -q 'kind: PersistentVolumeClaim' <<<"$out"; then fail "no claim created with existingClaim"; fi
grep -q 'claimName: mine' <<<"$out" || fail "existingClaim must be mounted"

# The Ingress never exposes the metrics.
out="$(render --set ingress.enabled=true --set ingress.host=tapyr.example.com)"
grep -q 'kind: Ingress' <<<"$out" || fail "the Ingress must render"
if grep -q 'path: /metrics' <<<"$out"; then fail "the Ingress must not expose /metrics"; fi

# A change of the configuration changes the pod template.
a="$(render --set config=a | grep checksum/config)"
b="$(render --set config=b | grep checksum/config)"
[[ "$a" != "$b" ]] || fail "the checksum must follow the configuration"

# The schema refuses a second replica and unknown values.
if render --set replicaCount=2 >/dev/null 2>&1; then fail "replicaCount must be refused"; fi
if render --set persistence.sise=2Gi >/dev/null 2>&1; then fail "unknown values must be refused"; fi

# The notes warn about an Ingress without authentication.
notes="$(helm install demo "$chart" --dry-run=client --namespace tapyr --set config=x --set ingress.enabled=true --set ingress.host=h 2>&1)"
grep -q 'WARNING: the Ingress is enabled without' <<<"$notes" || fail "the notes must warn about the Ingress"
notes="$(helm install demo "$chart" --dry-run=client --namespace tapyr --set config=x --set ingress.enabled=true --set ingress.host=h --set settings.server.basicAuth.username.env=U 2>&1)"
if grep -q 'WARNING: the Ingress is enabled without' <<<"$notes"; then fail "no warning with basicAuth"; fi

echo "helm check passed"
