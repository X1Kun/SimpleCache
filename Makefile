.PHONY: test race build vet check test-operator fmt-check test-ci test-operator-ci compose-config generated-check k8s-render
.PHONY: kind-up kind-smoke kind-delete
.PHONY: validate perf

# Prevent Go from invoking Git for automatic version stamping.
export GOFLAGS := $(GOFLAGS) -buildvcs=false

test:
	bash scripts/validation/run.sh unit
race:
	bash scripts/validation/run.sh race
build:
	cd geecache-engine && go build -o /dev/null ./cmd/simplecache
	cd simplecache-operator && go build -o /dev/null ./cmd
vet:
	cd geecache-engine && go vet ./...
	cd simplecache-operator && go vet ./cmd/... ./api/... ./internal/...
# Operator tests use a temporary API server and etcd, not a deployed cluster.
test-operator:
	bash scripts/validation/run.sh operator

fmt-check:
	test -z "$$(gofmt -l geecache-engine/cmd geecache-engine/internal simplecache-operator/api simplecache-operator/cmd simplecache-operator/internal simplecache-operator/test)"

test-ci:
	bash scripts/validation/run.sh race

test-operator-ci:
	bash scripts/validation/run.sh operator

compose-config:
	docker compose -f deploy/demo/compose.yaml config --quiet
	docker compose -f geecache-engine/docker-compose.yml config --quiet
	docker compose -f deploy/products/compose.yaml config --quiet

generated-check:
	bash scripts/check-generated.sh

k8s-render:
	$(MAKE) -C simplecache-operator kustomize
	simplecache-operator/bin/kustomize build simplecache-operator/config/default >/dev/null
	simplecache-operator/bin/kustomize build simplecache-operator/config/samples >/dev/null
	simplecache-operator/bin/kustomize build deploy/kind/operator >/dev/null
	simplecache-operator/bin/kustomize build deploy/gateway/operator >/dev/null
# Version-control operations belong to the user.
check: fmt-check test race build vet

kind-up:
	bash scripts/kind/up.sh

kind-smoke:
	bash scripts/validation/run.sh kind

validate:
	bash scripts/validation/run.sh local

# Manual separate-process investigation; default 10s x 3 repeats per scenario.
perf:
	bash scripts/validation/run.sh perf


kind-delete:
	bash scripts/kind/delete.sh

.PHONY: gateway-smoke
gateway-smoke:
	python3 -m tools.validation --suite smoke

.PHONY: gateway-experiments
gateway-experiments:
	python3 -m tools.validation --suite extended

.PHONY: gateway-test gateway-perf
gateway-test:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools/validation/tests

gateway-perf: gateway-test
	python3 -m tools.validation --suite performance

.PHONY: arrival-smoke
# Requires an explicitly selected disposable PostgreSQL database; owns its schema.
arrival-smoke:
	@test -n "$${SIMPLECACHE_TEST_DATABASE_URL:-}" || { echo "Set SIMPLECACHE_TEST_DATABASE_URL to an initialized disposable database" >&2; exit 1; }
	cd geecache-engine && go test -race -count=1 -timeout=2m -run '^TestArrivalRuntimeIntegration$$' -v ./internal/bench/gateway

.PHONY: gateway-arrival
gateway-arrival:
	python3 -m tools.validation --suite arrival

.PHONY: arrival-calibration
arrival-calibration:
	python3 -m tools.validation.arrival_calibration
