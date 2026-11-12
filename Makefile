GO           ?= go
CLUSTER      ?= kube-fence
IMAGE        ?= kube-fence:dev
KIND_NODE    ?=
KUBECTL      ?= kubectl --context kind-$(CLUSTER)

.PHONY: all test vet build image kind-up kind-down deploy e2e logs clean

all: vet test build

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

build:
	CGO_ENABLED=0 $(GO) build -o bin/kube-fence ./cmd/kube-fence

image:
	docker build -t $(IMAGE) .

kind-up: image
	@kind get clusters | grep -qx $(CLUSTER) || kind create cluster --name $(CLUSTER) $(if $(KIND_NODE),--image $(KIND_NODE))
	kind load docker-image $(IMAGE) --name $(CLUSTER)
	$(MAKE) deploy

deploy:
	$(KUBECTL) apply -f deploy/namespace.yaml -f deploy/config.yaml
	@CA=$$(KUBECTL="$(KUBECTL)" deploy/gen-certs.sh) && \
	$(KUBECTL) apply -f deploy/deployment.yaml && \
	$(KUBECTL) rollout restart deployment/kube-fence -n kube-fence && \
	$(KUBECTL) rollout status deployment/kube-fence -n kube-fence --timeout=120s && \
	sed "s|__CA_BUNDLE__|$$CA|g" deploy/webhooks.yaml | $(KUBECTL) apply -f -

e2e:
	KUBECTL="$(KUBECTL)" hack/e2e.sh

logs:
	$(KUBECTL) logs -n kube-fence deployment/kube-fence --tail=100

kind-down:
	kind delete cluster --name $(CLUSTER)

clean:
	$(GO) clean ./...
	rm -f bin/kube-fence
	rm -f deploy/certs/*
