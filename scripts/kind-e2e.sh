#!/usr/bin/env bash
# Real-cluster end-to-end test on kind. Unlike the unit/integration tests,
# this runs actual model-server pods and checks that:
#   1. ModelDock's client-go backend applies Deployment+Service+HPA and the
#      reconciler marks rollouts ready only when pods are really available;
#   2. staging -> approval -> prod -> rollback works against real rollouts,
#      and prod serves the rolled-back version (checked via /predict);
#   3. a broken image is marked failed after the rollout timeout and, with
#      auto-rollback on, staging returns to the last good version.
#
# Needs: docker, kind, kubectl, go, jq, curl. Runs Postgres+Kafka with
# docker compose and ModelDock itself on the host against the kind cluster.
#
#   ./scripts/kind-e2e.sh            # create cluster, run, keep cluster
#   KEEP=0 ./scripts/kind-e2e.sh     # delete cluster afterwards
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-modeldock}
KEEP=${KEEP:-1}
PORT=${PORT:-18080}
TIMEOUT=${ROLLOUT_TIMEOUT:-90s}
SECRET=dev-only-secret-change-me-0123456789
KCFG=$(mktemp)
LOG=$(mktemp)
PIDS=()

cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  [ "$KEEP" = 0 ] && kind delete cluster --name "$CLUSTER" || true
  echo "modeldock log: $LOG"
}
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { echo "FAIL: $*"; tail -50 "$LOG"; exit 1; }

step "kind cluster"
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind get kubeconfig --name "$CLUSTER" > "$KCFG"
export KUBECONFIG=$KCFG
kubectl get nodes

step "model-server image (1.0.0, 1.1.0 share the build; MODEL_VERSION comes from ModelDock)"
docker build -q -t modeldock/iris-server:1.0.0 model-server
docker tag modeldock/iris-server:1.0.0 modeldock/iris-server:1.1.0
kind load docker-image --name "$CLUSTER" modeldock/iris-server:1.0.0 modeldock/iris-server:1.1.0

step "Postgres + Kafka"
docker compose -f deploy/docker-compose.yml up -d --wait postgres kafka

step "real-cluster client test (kube.ClientGo, pods must become ready)"
MODELDOCK_TEST_KUBECONFIG=$KCFG MODELDOCK_TEST_EXPECT_READY=1 MODELDOCK_TEST_IMAGE=modeldock/iris-server:1.0.0 \
  go test -count=1 -run TestClientGoAgainstRealCluster -v ./kube/

step "ModelDock (kube=real, events=kafka, auto-rollback) on :$PORT"
go build -o bin/modeldock ./cmd/modeldock
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U modeldock -qc 'DROP DATABASE IF EXISTS modeldock_e2e WITH (FORCE)' -c 'CREATE DATABASE modeldock_e2e'
MODELDOCK_JWT_SECRET=$SECRET ./bin/modeldock serve -addr ":$PORT" \
  -database-url "postgres://modeldock:modeldock@localhost:5432/modeldock_e2e?sslmode=disable" \
  -kube real -kubeconfig "$KCFG" -events kafka -kafka-brokers localhost:29092 \
  -rollout-timeout "$TIMEOUT" -auto-rollback=true >"$LOG" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 30); do curl -sf "localhost:$PORT/readyz" >/dev/null && break; sleep 1; done
curl -sf "localhost:$PORT/readyz" >/dev/null || fail "modeldock did not become ready"

step "lifecycle: staging -> approval -> prod (x2) -> rollback"
API="http://localhost:$PORT" MODELDOCK_JWT_SECRET=$SECRET MODELDOCK_BIN=./bin/modeldock WAIT_SECS=180 ./scripts/demo.sh

step "pods really run the rolled-back version"
kubectl -n modeldock-prod rollout status deploy/iris --timeout=120s
kubectl -n modeldock-prod get deploy,svc,hpa,pods -o wide
kubectl -n modeldock-prod port-forward svc/iris 18082:80 >/dev/null 2>&1 &
PIDS+=($!)
for _ in $(seq 1 20); do curl -sf localhost:18082/health >/dev/null && break; sleep 1; done
GOT=$(curl -sf -XPOST localhost:18082/predict -H 'content-type: application/json' \
  -d '{"instances":[[5.1,3.5,1.4,0.2]]}' | jq -r '.model_version + " " + .predictions[0]')
echo "prod /predict -> $GOT"
[ "$GOT" = "1.0.0 setosa" ] || fail "expected prod to serve 1.0.0 after rollback, got '$GOT'"

step "failure path: unpullable image fails after $TIMEOUT and auto-rolls back staging"
DEP=$(MODELDOCK_JWT_SECRET=$SECRET ./bin/modeldock token -sub dana -role deployer)
H="Authorization: Bearer $DEP"
curl -sf -XPOST "localhost:$PORT/api/v1/models/iris/versions" -H "$H" \
  -d '{"version":"9.9.9","image":"modeldock/does-not-exist:9.9.9"}' >/dev/null
BAD=$(curl -sf -XPOST "localhost:$PORT/api/v1/models/iris/versions/9.9.9/deploy" -H "$H" -d '{}' | jq -r .id)
for _ in $(seq 1 150); do
  S=$(curl -sf "localhost:$PORT/api/v1/deployments/$BAD" -H "$H" | jq -r .status)
  [ "$S" = failed ] && break; sleep 2
done
curl -sf "localhost:$PORT/api/v1/deployments/$BAD" -H "$H" | jq -c '{status,reason}'
[ "$S" = failed ] || fail "bad deployment not marked failed (status $S)"
for _ in $(seq 1 90); do
  CUR=$(curl -sf "localhost:$PORT/api/v1/models/iris/status" -H "$H" | jq -r '.current.staging | .kind + " " + .version + " " + .status')
  [ "$CUR" = "auto_rollback 1.1.0 ready" ] && break; sleep 2
done
echo "staging now: $CUR"
[ "$CUR" = "auto_rollback 1.1.0 ready" ] || fail "expected staging auto-rolled back to 1.1.0, got '$CUR'"
kubectl -n modeldock-staging rollout status deploy/iris --timeout=120s

step "PASS"
