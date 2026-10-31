#!/usr/bin/env bash
# A slowed-down rollout to watch live in the web console.
#
#   FAKE_READY_POLLS=3 FAKE_FAIL_IMAGE=broken make compose-up
#   make demo-live            # then open http://localhost:8080 and paste a token
#
# It registers a fresh model, takes 1.0.0 and 1.1.0 through staging ->
# approval -> prod, deploys a broken 1.2.0 to staging (fails, auto-rolls
# back when the stack runs with FAKE_FAIL_IMAGE=broken), and finally rolls
# prod back. Approvals are done by this script unless APPROVE=browser, in
# which case it waits for you to click Approve in the console's queue.
# Requires curl and jq.
set -euo pipefail
API=${API:-http://localhost:8080}
SECRET=${MODELDOCK_JWT_SECRET:-dev-only-secret-change-me-0123456789}
MD=${MODELDOCK_BIN:-go run ./cmd/modeldock}
MODEL=${MODEL:-churn-$(date +%H%M%S)}
PAUSE=${PAUSE:-2}
APPROVE=${APPROVE:-auto}

tok() { MODELDOCK_JWT_SECRET=$SECRET $MD token -sub "$1" -role "$2" -ttl 2h; }
DEPLOYER=$(tok dana deployer)
APPROVER=$(tok alice approver)
VIEWER=$(tok vic viewer)

call() { # call TOKEN METHOD PATH [JSON]
  curl -sS -X "$2" "$API$3" -H "Authorization: Bearer $1" -H 'Content-Type: application/json' ${4:+-d "$4"}
}
wait_status() { # wait_status DEPLOYMENT_ID WANT
  for _ in $(seq 1 "${WAIT_SECS:-120}"); do
    s=$(call "$DEPLOYER" GET "/api/v1/deployments/$1" | jq -r .status)
    [ "$s" = "$2" ] && { echo "   -> $s"; return 0; }
    [ "$s" = failed ] && [ "$2" != failed ] && { echo "   -> failed"; return 1; }
    sleep 1
  done
  echo "timeout waiting for $1 to be $2"; return 1
}
step() { echo; echo "== $*"; sleep "$PAUSE"; }

cat <<MSG
Open the console: $API
Sign in with one of these tokens (valid 2h):

  approver alice: $APPROVER

  viewer vic:     $VIEWER

Then open Timeline and pick model "$MODEL" once it appears.
MSG
if [ -t 0 ]; then read -r -p "Press Enter to start the rollout... " _; fi

step "register $MODEL with versions 1.0.0, 1.1.0 and a broken 1.2.0"
call "$DEPLOYER" POST /api/v1/models "{\"name\":\"$MODEL\",\"description\":\"live demo model\"}" | jq -c '{name}'
reg() { # reg VERSION IMAGE_TAG MIN_REPLICAS
  call "$DEPLOYER" POST "/api/v1/models/$MODEL/versions" "{\"version\":\"$1\",\"image\":\"modeldock/iris-server:$2\",
    \"metadata\":{\"framework\":\"sklearn\",\"dataset\":\"churn-$1\"},
    \"resources\":{\"cpuRequest\":\"100m\",\"memoryRequest\":\"128Mi\",\"memoryLimit\":\"256Mi\"},
    \"scaling\":{\"minReplicas\":$3,\"maxReplicas\":4,\"targetCPUPercentage\":70}}" | jq -c '{version,image}'
}
reg 1.0.0 1.0.0 1
reg 1.1.0 1.1.0 2
reg 1.2.0 broken 2

for v in 1.0.0 1.1.0; do
  step "$v: deploy to staging (watch pending -> progressing -> ready)"
  D=$(call "$DEPLOYER" POST "/api/v1/models/$MODEL/versions/$v/deploy" '{"environment":"staging"}' | jq -r .id)
  wait_status "$D" ready
  step "$v: request promotion (shows up in the Approvals queue with its diff)"
  A=$(call "$DEPLOYER" POST "/api/v1/models/$MODEL/versions/$v/promotion-requests" "{\"comment\":\"$v green in staging\"}" | jq -r .id)
  if [ "$APPROVE" = browser ]; then
    echo "   approve it in the console as alice (Approvals page)..."
    until [ "$(call "$VIEWER" GET "/api/v1/approvals/$A" | jq -r .status)" != pending ]; do sleep 1; done
  else
    sleep "$PAUSE"
    call "$APPROVER" POST "/api/v1/approvals/$A/approve" '{"comment":"lgtm"}' | jq -c '{status,decidedBy}'
  fi
  step "$v: promote to prod"
  P=$(call "$DEPLOYER" POST "/api/v1/models/$MODEL/versions/$v/promote" | jq -r .id)
  wait_status "$P" ready
done

step "1.2.0: deploy a broken image to staging (fails; auto-rollback to 1.1.0 if FAKE_FAIL_IMAGE=broken)"
D=$(call "$DEPLOYER" POST "/api/v1/models/$MODEL/versions/1.2.0/deploy" '{"environment":"staging"}' | jq -r .id)
wait_status "$D" failed || true
sleep "$PAUSE"
call "$DEPLOYER" GET "/api/v1/models/$MODEL/status" | jq -c '.current.staging | {version,kind,status}'

step "rollback prod to the previous known-good version"
R=$(call "$DEPLOYER" POST "/api/v1/models/$MODEL/rollback" '{"environment":"prod"}' | jq -r .id)
wait_status "$R" ready
call "$DEPLOYER" GET "/api/v1/models/$MODEL/status" | jq -c '.current | map_values({version,status,kind,revision})'
echo; echo "done: $API/#/timeline?model=$MODEL"
