SHELL := /bin/bash
GO ?= go
BIN := bin/modeldock
GOBIN ?= $(shell $(GO) env GOPATH)/bin
STATICCHECK_VERSION ?= 2024.1.1
KUBECONFORM_VERSION ?= v0.6.7
K8S_VERSION ?= 1.30.0
COMPOSE := docker compose -f deploy/docker-compose.yml
PY ?= python3
VENV := model-server/.venv

# Postgres/Kafka from `make deps-up`. DB tests create and drop their own
# databases, so the user needs CREATEDB (the compose user is a superuser).
export MODELDOCK_TEST_DATABASE_URL ?= postgres://modeldock:modeldock@localhost:5432/modeldock?sslmode=disable
export MODELDOCK_TEST_KAFKA_BROKERS ?= localhost:29092
export MODELDOCK_JWT_SECRET ?= dev-only-secret-change-me-0123456789

.PHONY: help setup generate build test test-unit test-race lint fmt manifests \
        model-server-test deps-up deps-down run compose-up compose-down demo loadtest kind-e2e images clean

help: ## list targets
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

setup: ## download Go modules, install lint/validation tools, create the model-server venv
	$(GO) mod download
	$(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	$(GO) install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)
	$(PY) -m venv $(VENV) && $(VENV)/bin/pip install -q -r model-server/requirements-dev.txt

generate: ## regenerate gqlgen code from api/graph/schema.graphqls
	cd api/graph && $(GO) run github.com/99designs/gqlgen generate

build: ## build the modeldock binary
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN) ./cmd/modeldock

test: deps-up ## all Go tests incl. Postgres + Kafka integration (starts deps)
	$(GO) test -count=1 ./...

test-race: deps-up ## all Go tests with the race detector
	$(GO) test -race -count=1 ./...

test-unit: ## Go tests that need no external services (DB/Kafka tests skip)
	env -u MODELDOCK_TEST_DATABASE_URL -u MODELDOCK_TEST_KAFKA_BROKERS $(GO) test -count=1 ./...

model-server-test: ## pytest for the sample model server
	$(VENV)/bin/python -m pytest -q model-server/tests

fmt: ## gofmt the tree
	gofmt -w $$(git ls-files '*.go')

lint: ## gofmt check, go vet, staticcheck (+ golangci-lint if installed)
	@test -z "$$(gofmt -l $$(git ls-files '*.go'))" || (gofmt -l $$(git ls-files '*.go'); echo "run make fmt"; exit 1)
	$(GO) vet ./...
	$(GOBIN)/staticcheck ./...
	@if command -v golangci-lint >/dev/null; then golangci-lint run ./...; else echo "golangci-lint not installed, skipped"; fi

manifests: build ## validate generated model manifests and the Helm chart with kubeconform
	$(BIN) render > /tmp/modeldock-render.yaml
	$(GOBIN)/kubeconform -strict -summary -kubernetes-version $(K8S_VERSION) /tmp/modeldock-render.yaml
	helm lint deploy/helm/modeldock --set secret.databaseUrl=x --set secret.jwtSecret=$(MODELDOCK_JWT_SECRET)
	helm template md deploy/helm/modeldock --set secret.databaseUrl=x --set secret.jwtSecret=$(MODELDOCK_JWT_SECRET) \
	  | $(GOBIN)/kubeconform -strict -summary -kubernetes-version $(K8S_VERSION)

deps-up: ## start Postgres and Kafka (docker compose)
	$(COMPOSE) up -d --wait postgres kafka

deps-down:
	$(COMPOSE) stop postgres kafka

run: deps-up build ## run the API on the host (fake kube, Kafka events)
	$(BIN) serve -database-url "$(MODELDOCK_TEST_DATABASE_URL)" -events kafka -kafka-brokers $(MODELDOCK_TEST_KAFKA_BROKERS) -kube fake -auto-rollback

compose-up: ## full local stack: Postgres, Kafka, API, dashboard consumer
	$(COMPOSE) up -d --build --wait

compose-down: ## stop the stack and delete its volumes
	$(COMPOSE) down -v

demo: ## curl/GraphQL walkthrough against localhost:8080
	MODELDOCK_BIN=$(BIN) ./scripts/demo.sh

loadtest: ## vegeta load test against localhost:8080 (run `make demo` first)
	MODELDOCK_BIN=$(BIN) ./loadtest/run.sh

kind-e2e: ## real-cluster end-to-end test on kind
	./scripts/kind-e2e.sh

images: ## build the modeldock and model-server images
	docker build -f deploy/Dockerfile -t modeldock/modeldock:dev .
	docker build -t modeldock/iris-server:1.0.0 model-server

clean:
	rm -rf bin loadtest/out
