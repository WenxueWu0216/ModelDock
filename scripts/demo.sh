#!/usr/bin/env bash
# End-to-end walkthrough against a running ModelDock (default: docker compose).
# Registers iris 1.0.0 and 1.1.0, deploys to staging, gets approval, promotes,
# then rolls prod back in one call. Requires curl and jq.
set -euo pipefail
API=${API:-http://localhost:8080}
SECRET=${MODELDOCK_JWT_SECRET:-dev-only-secret-change-me-0123456789}
MD=${MODELDOCK_BIN:-go run ./cmd/modeldock}

tok() { MODELDOCK_JWT_SECRET=$SECRET $MD token -sub "$1" -role "$2"; }
DEPLOYER=$(tok dana deployer)
APPROVER=$(tok alice approver)

call() { # call TOKEN METHOD PATH [JSON]
  curl -sS -X "$2" "$API$3" -H "Authorization: Bearer $1" -H 'Content-Type: application/json' ${4:+-d "$4"}
}
wait_ready() { # wait_ready DEPLOYMENT_ID
  for _ in $(seq 1 "${WAIT_SECS:-60}"); do
    s=$(call "$DEPLOYER" GET "/api/v1/deployments/$1" | jq -r .status)
    [ "$s" = ready ] && return 0
    [ "$s" = failed ] && { echo "deployment $1 failed"; return 1; }
    sleep 1
  done
  echo "timeout waiting for $1"; return 1
}

echo "== register model and two versions"
call "$DEPLOYER" POST /api/v1/models '{"name":"iris","description":"Iris classifier"}' | jq -c '{name,createdBy}'
for v in 1.0.0 1.1.0; do
  call "$DEPLOYER" POST /api/v1/models/iris/versions "{\"version\":\"$v\",\"image\":\"modeldock/iris-server:$v\",
    \"metadata\":{\"framework\":\"sklearn\",\"dataset\":\"iris\"},
    \"resources\":{\"cpuRequest\":\"100m\",\"memoryRequest\":\"128Mi\",\"memoryLimit\":\"256Mi\"},
    \"scaling\":{\"minReplicas\":1,\"maxReplicas\":4,\"targetCPUPercentage\":70}}" | jq -c '{version,state}'
done

for v in 1.0.0 1.1.0; do
  echo "== $v: staging -> approval -> prod"
  D=$(call "$DEPLOYER" POST "/api/v1/models/iris/versions/$v/deploy" '{"environment":"staging"}' | jq -r .id)
  wait_ready "$D"
  A=$(call "$DEPLOYER" POST "/api/v1/models/iris/versions/$v/promotion-requests" '{"comment":"staging green"}' | jq -r .id)
  echo -n "self-approval by deployer -> "; call "$DEPLOYER" POST "/api/v1/approvals/$A/approve" '{}' | jq -c .
  call "$APPROVER" POST "/api/v1/approvals/$A/approve" '{"comment":"lgtm"}' | jq -c '{status,decidedBy}'
  P=$(call "$DEPLOYER" POST "/api/v1/models/iris/versions/$v/promote" | jq -r .id)
  wait_ready "$P"
done

echo "== diff 1.0.0 -> 1.1.0"
call "$DEPLOYER" GET '/api/v1/models/iris/diff?from=1.0.0&to=1.1.0' | jq -c .changes

echo "== rollback prod (one call)"
R=$(call "$DEPLOYER" POST /api/v1/models/iris/rollback '{"environment":"prod"}' | jq -r .id)
wait_ready "$R"
call "$DEPLOYER" GET /api/v1/models/iris/status | jq -c '.current | map_values({version,status,kind,revision})'

echo "== prod history"
call "$DEPLOYER" GET '/api/v1/models/iris/deployments?environment=prod' | jq -c '.[] | {revision,version,kind,status}'

echo "== GraphQL: one round trip"
call "$APPROVER" POST /graphql '{"query":"{ model(name: \"iris\") { versions { version state } prod { version status } } audit(model: \"iris\", limit: 3) { type actor } }"}' | jq -c .data

echo "== dashboard (built by the Kafka consumer; may lag a second)"
sleep 2
call "$DEPLOYER" GET /api/v1/dashboard | jq -c '.[]'
