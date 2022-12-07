REGISTRY ?= localhost:5001
export KUBECONFIG := .out/kubeconfig

build:
	DOCKER_BUILDKIT=1 docker build . -t ${REGISTRY}/ovn-kubevirt
push: build
	DOCKER_BUILDKIT=1 docker push ${REGISTRY}/ovn-kubevirt
run: 
	hack/kind.sh run

apply: push
	kubectl apply -f ovn-kubevirt.yaml

delete: 
	kubectl exec $(shell hack/node worker) -c ovs-server -- ovn-kube-util bridges-to-nic breth0
	kubectl exec $(shell hack/node worker2) -c ovs-server -- ovn-kube-util bridges-to-nic breth0
	kubectl delete --wait=true --cascade=foreground -f ovn-kubevirt.yaml --ignore-not-found

install: plugin
	hack/kind.sh install-cni-plugin

test: install
	kubectl delete --ignore-not-found -f hack/test.yaml
	kubectl apply -f hack/test.yaml

plugin:
	hack/kind.sh build-cni-plugin

sync: delete apply
	kubectl rollout status deployment/ovn-kubevirt-control-plane
	kubectl rollout status ds/ovn-kubevirt-node

logs:
	kubectl logs -l app=ovn-kubevirt --all-containers --tail=100000
