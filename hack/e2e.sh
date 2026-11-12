#!/usr/bin/env bash
# End-to-end scenarios against a kind cluster prepared by `make kind-up`.
# Agent writes are simulated by impersonating system:serviceaccount:agents:remediator.
set -uo pipefail

KUBECTL=${KUBECTL:-kubectl --context kind-kube-fence}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
PAUSE=registry.k8s.io/pause:3.10
K="$KUBECTL"
A="$KUBECTL --as=system:serviceaccount:agents:remediator"
PASS=0
FAIL=0

say() { printf '\n== %s\n' "$*"; }

expect_allow() {
  local desc=$1; shift
  local out
  if out=$("$@" 2>&1); then
    PASS=$((PASS + 1)); printf 'PASS  allow  %s\n' "$desc"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL  allow  %s\n      %s\n' "$desc" "$out"
  fi
  [[ -n "${SHOW:-}" ]] && printf '      %s\n' "$out"
  return 0
}

expect_deny() {
  local desc=$1 want=$2; shift 2
  local out
  if out=$("$@" 2>&1); then
    FAIL=$((FAIL + 1)); printf 'FAIL  deny   %s (write was allowed)\n' "$desc"
  elif grep -q -- "$want" <<<"$out"; then
    PASS=$((PASS + 1)); printf 'PASS  deny   %s\n      %s\n' "$desc" "$(head -c 400 <<<"$out")"
  else
    FAIL=$((FAIL + 1)); printf 'FAIL  deny   %s (missing %q)\n      %s\n' "$desc" "$want" "$out"
  fi
  return 0
}

restart_fence() {
  $K rollout restart deployment/kube-fence -n kube-fence >/dev/null
  $K rollout status deployment/kube-fence -n kube-fence --timeout=120s >/dev/null
  for _ in $(seq 1 60); do
    $A create configmap kube-fence-probe -n demo --dry-run=server >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "webhook did not become reachable" >&2
  return 1
}

wait_for_pod() {
  for _ in $(seq 1 60); do
    if [[ "$($K get pod "$1" -n demo -o jsonpath='{.metadata.uid}' 2>/dev/null)" != "${2:-}" ]] &&
      $K get pod "$1" -n demo >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for pod $1" >&2
  return 1
}

say "setup"
$K delete namespace demo --ignore-not-found --wait=true >/dev/null
$K apply -f "$ROOT/deploy/examples/agent-rbac.yaml" >/dev/null
restart_fence

argocd_sync() {
  cat <<EOF | $K apply --server-side --force-conflicts --field-manager=argocd-controller -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
  labels: {app: web}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
        - name: app
          image: $PAUSE
EOF
}
argocd_sync
$K create deployment api -n demo --image=$PAUSE --replicas=1 >/dev/null
$K patch deployment api -n demo --subresource=scale --type=merge -p '{"spec":{"replicas":2}}' \
  --field-manager=kube-controller-manager >/dev/null
$K create deployment worker -n demo --image=$PAUSE --replicas=1 >/dev/null
cat <<EOF | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: demo}
spec:
  serviceName: db
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec:
      containers: [{name: db, image: $PAUSE}]
EOF
for cm in a b c; do $K create configmap "$cm" -n demo --from-literal=k=v >/dev/null; done
wait_for_pod db-0
for d in web api; do
  echo "managers on deployment/$d:"
  $K get deployment "$d" -n demo -o jsonpath='{range .metadata.managedFields[*]}  {.manager} {.operation} {.subresource}{"\n"}{end}'
done

say "ownership guard"
expect_deny "agent sets spec.replicas owned by the HPA (scale subresource)" "owned by kube-controller-manager via /scale" \
  $A patch deployment api -n demo --type=merge -p '{"spec":{"replicas":5}}'
expect_deny "agent changes image applied by argocd-controller" "owned by argocd-controller" \
  $A set image deployment/web -n demo app=registry.k8s.io/pause:3.9
expect_allow "same image change with an override set by this request" \
  $A patch deployment web -n demo --type=merge \
  -p '{"metadata":{"annotations":{"kube-fence.io/allow-ownership-override":"INC-1042"}},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"registry.k8s.io/pause:3.9"}]}}}}'
argocd_sync
expect_deny "agent re-applies the same image after argocd reverted it" "duplicate irreversible effect" \
  $A set image deployment/web -n demo app=registry.k8s.io/pause:3.9
expect_deny "stale override left on the object does not cover a new write" "\[field-ownership\]" \
  $A set image deployment/web -n demo app=registry.k8s.io/pause:3.8
echo "provenance annotation on deployment/web:"
echo "  $($K get deployment web -n demo -o jsonpath='{.metadata.annotations.kube-fence\.io/last-agent-write}')"

say "effect ledger"
uid=$($K get pod db-0 -n demo -o jsonpath='{.metadata.uid}')
expect_allow "agent restarts db-0" $A delete pod db-0 -n demo --wait=false
wait_for_pod db-0 "$uid"
expect_deny "retried restart of the recreated db-0" "duplicate irreversible effect" \
  $A delete pod db-0 -n demo --wait=false

migration() {
  $A create -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  generateName: migration-run-
  namespace: demo
  annotations: {kube-fence.io/intent-id: "$1"}
data: {step: "drop-old-index"}
EOF
}
expect_allow "agent creates generateName object with intent-id ticket-7" migration ticket-7
expect_deny "retry with the same intent-id" "intent:ticket-7" migration ticket-7
expect_allow "new intent-id with identical body" migration ticket-8

backfill() {
  $A create -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata: {generateName: backfill-run-, namespace: demo}
data: {step: "backfill-orders"}
EOF
}
expect_allow "agent creates generateName object without intent-id" backfill
expect_deny "retry gets a new random name but the same content key" "duplicate irreversible effect" backfill

say "blast radius"
expect_deny "agent scales worker 1 -> 10 via /scale (cap 3)" "exceeds max delta 3" \
  $A scale deployment worker -n demo --replicas=10
expect_allow "agent scales worker 1 -> 3" $A scale deployment worker -n demo --replicas=3
expect_allow "agent deletes configmap a (2nd delete in window)" $A delete configmap a -n demo
expect_allow "agent deletes configmap b (3rd delete in window)" $A delete configmap b -n demo
expect_deny "agent deletes configmap c (over cap of 3)" "delete-limit" $A delete configmap c -n demo
expect_allow "human deletes configmap c" $K delete configmap c -n demo
expect_allow "agent repeats an absolute scale to 3 (no-op, allowed)" \
  $A scale deployment worker -n demo --replicas=3

say "audit mode"
$K get configmap kube-fence-config -n kube-fence -o yaml | sed 's/mode: enforce/mode: audit/' | $K apply -f - >/dev/null
restart_fence
uid=$($K get pod db-0 -n demo -o jsonpath='{.metadata.uid}')
expect_allow "audit: first delete" $A delete pod db-0 -n demo --wait=false
wait_for_pod db-0 "$uid"
out=$($A delete pod db-0 -n demo --wait=false 2>&1)
if [[ $? -eq 0 ]] && grep -q "(audit) would deny" <<<"$out"; then
  PASS=$((PASS + 1)); printf 'PASS  audit  duplicate delete allowed with warning\n      %s\n' "$(head -c 400 <<<"$out")"
else
  FAIL=$((FAIL + 1)); printf 'FAIL  audit  duplicate delete\n      %s\n' "$out"
fi
$K get configmap kube-fence-config -n kube-fence -o yaml | sed 's/mode: audit/mode: enforce/' | $K apply -f - >/dev/null
restart_fence

say "result: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
