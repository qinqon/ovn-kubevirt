REGISTRY ?= localhost:5001
export KUBECONFIG := .out/kubeconfig

.PHONY: build
build:
	DOCKER_BUILDKIT=1 docker build . -t ${REGISTRY}/ovn-kubevirt

.PHONY: push
push: build
	DOCKER_BUILDKIT=1 docker push ${REGISTRY}/ovn-kubevirt

.PHONY: cluster-up
cluster-up: 
	hack/kind.sh run

.PHONY: cluster-sync
cluster-sync:
	hack/kind.sh deploy

.PHONY: test
test:
	kubectl delete --ignore-not-found -f test/cluster1.yaml
	kubectl apply -f test/cluster1.yaml

.PHONY: capk
install-capk:
	hack/kind.sh install-capk
	
.PHONY: capk-cluster
tenant-cluster: capk
	hack/kind.sh create-tenant-cluster
