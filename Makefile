REGISTRY ?= localhost:5001
export KUBECONFIG := .out/kubeconfig

build:
	DOCKER_BUILDKIT=1 docker build . -t ${REGISTRY}/ovn-kubevirt
push: build
	DOCKER_BUILDKIT=1 docker push ${REGISTRY}/ovn-kubevirt
run: 
	hack/kind.sh run

apply: push
	hack/kind.sh install-ovn-kubevirt

delete: 
	kubectl delete -f ovn-kubevirt.yaml --ignore-not-found

install: plugin
	hack/kind.sh install-cni-plugin

test: install
	kubectl delete --ignore-not-found -f hack/test.yaml
	kubectl apply -f hack/test.yaml

plugin:
	hack/kind.sh build-cni-plugin

sync: delete apply

logs:
	kubectl logs -l app=ovn-kubevirt --all-containers --tail=100000
