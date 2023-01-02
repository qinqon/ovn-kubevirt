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
cluster-sync: plugin
	hack/kind.sh deploy

.PHONY: test
test:
	kubectl delete --ignore-not-found -f test/cluster1.yaml
	kubectl apply -f test/cluster1.yaml

.PHONY: plugin
plugin:
	hack/kind.sh build-cni-plugin

.PHONY: capk
capk:
	hack/kind.sh install-capi
	hack/kind.sh install-capk
	hack/kind.sh wait-capk
	
.PHONY: capk-cluster
capk-cluster: capk
	hack/kind.sh create-capk-cluster
