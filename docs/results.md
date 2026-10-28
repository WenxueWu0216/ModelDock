# Load test results

These numbers come from real runs of [`loadtest/run.sh`](../loadtest/run.sh)
(vegeta 12.12) made while building this project. None are estimates. The raw
vegeta text reports and latency histograms are in
[`docs/loadtest/`](loadtest/).

## Setup (read this before quoting the numbers)

| | |
|---|---|
| Host | One sandbox VM, 4 vCPU (Intel Xeon @ 2.80GHz), 15 GiB RAM |
| Stack | `deploy/docker-compose.yml`: Postgres 16.15, Kafka 3.8.0 (KRaft, 1 broker), ModelDock API built from `deploy/Dockerfile` with the `golang:1.22` builder, dashboard consumer |
| Kubernetes | **In-memory fake** (`MODELDOCK_KUBE=fake`). No cluster was involved, so these figures measure the control-plane API, Postgres and the outbox, not rollouts. |
| Load generator | vegeta on the **same VM**. It competes for the same 4 cores, and traffic goes through Docker's userland port proxy. |
| Data | `scripts/demo.sh` seeded model `iris` with 2 versions and 3 prod + 2 staging revisions. The write scenario then adds 3,000 versions. |
| Auth | Every request carries a JWT and is verified (HS256). |

A single shared VM is a pessimistic environment. Treat the results as a
regression baseline for this codebase, not as capacity planning numbers.

## Final run (run 3)

Each scenario runs at a constant rate for 30 s. Latencies are client-observed.

| Scenario | Rate | Requests | Success | p50 | p90 | p99 | max |
|---|---:|---:|---:|---:|---:|---:|---:|
| `GET /api/v1/models/iris/status` (REST read, 3 queries) | 300/s | 9,000 | 100% | **1.90 ms** | 2.60 ms | **5.08 ms** | 30.2 ms |
| `GET /api/v1/models/iris/deployments?limit=20` (REST read) | 300/s | 9,000 | 100% | **1.82 ms** | 2.48 ms | **5.38 ms** | 38.8 ms |
| `POST /graphql` (model + versions + staging + prod + history) | 300/s | 9,000 | 100% | **2.63 ms** | 5.01 ms | **17.1 ms** | 251 ms |
| `POST /api/v1/models/iris/versions` (write: lock + insert + audit + outbox, one tx) | 100/s | 3,000 | 100% | **3.88 ms** | 5.37 ms | **10.6 ms** | 29.3 ms |
| `POST /graphql`, after the write scenario (`versions` now returns 50 rows) | 150/s | 4,500 | 100% | 6.31 ms | 10.5 ms | 25.3 ms | 86.7 ms |

Outbox after the run, measured in Postgres:

- 3,017 audit events and 3,017 outbox rows published. Backlog 0, publish errors 0.
- Commit-to-Kafka-ack lag (`published_at - created_at`): **p50 130 ms, p99 459 ms**.
  Most of this is the relay's 500 ms poll interval. Polling trades latency for
  simplicity; `LISTEN/NOTIFY` would cut it (see the README's limitations).

## What the load test found (runs 1 and 2)

The first run surfaced two real defects, both now fixed.

**1. GraphQL p50 was 599 ms (REST p50 was 2 ms).** Run 1, k3s running in the background:

| Scenario | p50 | p99 |
|---|---:|---:|
| REST status | 2.11 ms | 27.5 ms |
| GraphQL | **599 ms** | **1.23 s** |
| Write | 4.85 ms | 31.5 ms |

`pg_stat_activity` showed only about 3 active API connections under load.
Two causes:

- pgx's default pool is `max(4, NumCPU)` = 4 connections.
- The outbox relay and the reconciler each held one **pooled** connection
  for as long as they held their advisory leader lock. That left 2
  connections for gqlgen's concurrently executed field resolvers, at roughly
  7 to 10 queries per GraphQL request.

Fix (commit "store: leader locks on dedicated connections..."):

- Leader locks now live on dedicated connections outside the pool. A test
  asserts that holding leadership acquires 0 pooled connections.
- The pool defaults to 20 connections (`pool_max_conns` in the URL overrides it).
- The `staging`/`prod` resolvers fetch one environment instead of the whole
  status.

Run 2 (fix applied, k3s still running): GraphQL p50 **6.8 ms**, p99 338 ms.

**2. GraphQL `versions` was unbounded.** After the write scenario added
3,000 versions, every GraphQL request returned all of them. The follow-up
run had a 13% success rate: the host's Docker port proxy for `:8080` died
under the response volume. `versions` now takes a `limit` (default 50,
max 500) in both REST and GraphQL.

Runs 1 and 2 also shared the VM with a k3s server whose pod sandboxes kept
failing (see README, "Verification status"). Its retry loop used ~45% CPU in
k3s plus ~80% in dockerd, so REST tail latencies from those runs are noisy.
k3s was stopped for run 3. The GraphQL gap (599 ms vs 2.6 ms) is far larger
than that noise, and pool statistics confirmed its cause.

## Reproduce

```sh
make compose-up                        # Postgres, Kafka, API, consumer
./scripts/demo.sh                      # seed model "iris"
RATE_READ=300 RATE_WRITE=100 DURATION=30s ./loadtest/run.sh
```

## Not measured

- Rollout duration (`modeldock_deploy_duration_seconds`) against a real
  cluster. With the fake client, a rollout is "ready" one reconcile tick
  after apply.
- Multi-replica API behaviour under load (leader election is unit-tested only).
- Kafka with more than one broker, or with `acks=all` across replicas.
