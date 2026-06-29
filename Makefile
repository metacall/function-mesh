CLUSTER_NAME   ?= metacall-mesh
HELM_RELEASE   ?= function-mesh
HELM_CHART     ?= deploy/function-mesh

OPERATOR_IMAGE ?= localhost:5000/metacall/function-mesh-operator:dev
API_IMAGE      ?= localhost:5000/metacall/function-mesh-api:dev
ROUTER_IMAGE   ?= localhost:5000/metacall/function-mesh-router:dev
RUNTIME_IMAGE  ?= localhost:5000/metacall/function
LANG_RUNTIME   ?= metacall/core:latest
LANGUAGES      ?= py rb node wasm java

## One-command dev setup: cluster + build + deploy + port-forward
up: cluster-up deploy

## Create kind cluster + registry + Cilium CNI
cluster-up:
	bash deploy/kind/setup.sh

## Restart stopped kind containers (after reboot / Docker restart)
cluster-start:
	docker start $(CLUSTER_NAME)-control-plane $(CLUSTER_NAME)-worker $(CLUSTER_NAME)-worker2
	@echo "Waiting for API server…"
	@sleep 5
	kubectl cluster-info

## Stop kind containers without deleting the cluster
cluster-stop:
	docker stop $(CLUSTER_NAME)-control-plane $(CLUSTER_NAME)-worker $(CLUSTER_NAME)-worker2

## Delete the kind cluster entirely
cluster-down:
	kind delete cluster --name $(CLUSTER_NAME)

## Print cluster / node / pod status
cluster-status:
	@echo "=== Cluster ==="
	@kubectl cluster-info 2>/dev/null || echo "Cluster unreachable"
	@echo ""
	@echo "=== Nodes ==="
	@kubectl get nodes -o wide 2>/dev/null || true
	@echo ""
	@echo "=== Pods (all namespaces) ==="
	@kubectl get pods -A 2>/dev/null || true

build-operator:
	go build ./cmd/operator

build-api:
	go build ./cmd/api

build-router:
	go build ./cmd/router

build: build-operator build-api build-router

docker-build-operator:
	docker build -f Dockerfile.operator -t $(OPERATOR_IMAGE) .

docker-build-api:
	docker build -f Dockerfile.api -t $(API_IMAGE) .

docker-build-router:
	docker build -f Dockerfile.router -t $(ROUTER_IMAGE) .

docker-build-runtime:
	@for lang in $(LANGUAGES); do \
		docker build --build-arg LANG_RUNTIME=$(LANG_RUNTIME) \
			-f runtime/Dockerfile -t $(RUNTIME_IMAGE):$$lang runtime; \
	done

docker-build: docker-build-operator docker-build-api docker-build-router docker-build-runtime

docker-push-operator:
	docker push $(OPERATOR_IMAGE)

docker-push-api:
	docker push $(API_IMAGE)

docker-push-router:
	docker push $(ROUTER_IMAGE)

docker-push-runtime:
	@for lang in $(LANGUAGES); do \
		docker push $(RUNTIME_IMAGE):$$lang; \
	done

docker-push: docker-push-operator docker-push-api docker-push-router docker-push-runtime

deploy: docker-build docker-push helm-install wait

helm-install:
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART)

## Wait for all control-plane deployments to be ready
wait:
	kubectl -n metacall-system rollout status deploy/function-mesh-operator --timeout=180s
	kubectl -n metacall-system rollout status deploy/function-mesh-router  --timeout=180s
	kubectl -n metacall-system rollout status deploy/function-mesh-api     --timeout=180s

## Uninstall Helm release and delete leftover cluster-scoped resources
clean:
	-helm uninstall $(HELM_RELEASE) 2>/dev/null
	-kubectl delete clusterrole function-mesh-operator function-mesh-api function-mesh-router 2>/dev/null
	-kubectl delete clusterrolebinding function-mesh-operator function-mesh-api function-mesh-router 2>/dev/null
	-kubectl delete crd functions.metacall.io 2>/dev/null
	-kubectl delete namespace metacall-system metacall-functions 2>/dev/null

# ──────────────────────────────────────────────────────────────
#  5. Dev helpers
# ──────────────────────────────────────────────────────────────


## Forward Hubble UI to localhost:12000
hubble-ui:
	cilium hubble ui

## Observe mesh network flows
hubble-observe:
	hubble observe --namespace metacall-functions

# ──────────────────────────────────────────────────────────────
#  6. Tests
# ──────────────────────────────────────────────────────────────

test:
	go test ./...

test-controller:
	go test ./internal/controller

test-router:
	go test ./internal/router

test-api:
	go test ./internal/api

# ──────────────────────────────────────────────────────────────

.PHONY: up cluster-up cluster-start cluster-stop cluster-down cluster-status \
        build build-operator build-api build-router \
        docker-build docker-build-operator docker-build-api docker-build-router docker-build-runtime \
        docker-push docker-push-operator docker-push-api docker-push-router docker-push-runtime \
        deploy helm-install wait clean \
        hubble-ui hubble-observe \
        test test-controller test-router test-api
