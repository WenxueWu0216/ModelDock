#!/usr/bin/env bash
# Load test the ModelDock API with vegeta (https://github.com/tsenart/vegeta).
# Assumes a running API (e.g. `make compose-up`) with model "iris" registered
# (scripts/demo.sh does that). Writes raw results and text reports to
# loadtest/out/.
#
#   RATE_READ=300 RATE_WRITE=100 DURATION=30s ./loadtest/run.sh
set -euo pipefail
API=${API:-http://localhost:8080}
SECRET=${MODELDOCK_JWT_SECRET:-dev-only-secret-change-me-0123456789}
MD=${MODELDOCK_BIN:-go run ./cmd/modeldock}
RATE_READ=${RATE_READ:-300}
RATE_WRITE=${RATE_WRITE:-100}
DURATION=${DURATION:-30s}
OUT=${OUT:-loadtest/out}
mkdir -p "$OUT"

TOKEN=$(MODELDOCK_JWT_SECRET=$SECRET $MD token -sub loadtest -role deployer)
AUTH="Authorization: Bearer $TOKEN"

run() { # run NAME RATE TARGETS_FILE [extra vegeta attack flags]
  local name=$1 rate=$2 targets=$3; shift 3
  echo "== $name @ ${rate}/s for $DURATION"
  vegeta attack -rate="$rate" -duration="$DURATION" -timeout=10s -max-workers=256 -targets="$targets" "$@" \
    | tee "$OUT/$name.bin" | vegeta report -type=text | tee "$OUT/$name.txt"
  vegeta report -type='hist[0,2ms,5ms,10ms,25ms,50ms,100ms,250ms]' < "$OUT/$name.bin" > "$OUT/$name.hist.txt"
}

# 1. Read: current status per environment (3 queries).
printf 'GET %s/api/v1/models/iris/status\n%s\n' "$API" "$AUTH" > "$OUT/status.targets"
run read_status "$RATE_READ" "$OUT/status.targets"

# 2. Read: deployment history.
printf 'GET %s/api/v1/models/iris/deployments?limit=20\n%s\n' "$API" "$AUTH" > "$OUT/history.targets"
run read_history "$RATE_READ" "$OUT/history.targets"

# 3. Read: GraphQL query fetching model, versions, status and history at once.
GQL='{"query":"{ model(name: \"iris\") { name versions { version state } staging { version status } prod { version status } deployments(limit: 5) { revision status } } }"}'
echo "$GQL" > "$OUT/gql.json"
printf 'POST %s/graphql\n%s\nContent-Type: application/json\n@%s\n' "$API" "$AUTH" "$OUT/gql.json" > "$OUT/gql.targets"
run read_graphql "$RATE_READ" "$OUT/gql.targets"

# 4. Write: register a new version per request. Each is one transaction:
#    model row lock + insert version + audit event + outbox row.
RUN_ID=$(date +%s)
N=$(( RATE_WRITE * ${DURATION%s} + 100 ))
python3 - "$API" "$TOKEN" "$RUN_ID" "$N" > "$OUT/register.targets" <<'PY'
import base64, json, sys
api, token, run_id, n = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
for i in range(n):
    body = json.dumps({"version": f"lt-{run_id}-{i}", "image": f"modeldock/iris-server:lt-{i}",
                       "metadata": {"source": "loadtest"}})
    print(json.dumps({"method": "POST", "url": f"{api}/api/v1/models/iris/versions",
                      "header": {"Authorization": [f"Bearer {token}"], "Content-Type": ["application/json"]},
                      "body": base64.b64encode(body.encode()).decode()}))
PY
run write_register "$RATE_WRITE" "$OUT/register.targets" -format=json

echo "Reports in $OUT/"
