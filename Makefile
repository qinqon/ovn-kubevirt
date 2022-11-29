export KUBECONFIG := .out/kubeconfig

build:
	docker build . -t quay.io/ellorent/ovn-kubevirt
push: build
	docker push quay.io/ellorent/ovn-kubevirt
run: 
	hack/kind.sh run
apply:
	kubectl apply -f ovn-kubevirt.yaml

delete: 
	kubectl delete -f ovn-kubevirt.yaml

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
