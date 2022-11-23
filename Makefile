export KUBECONFIG := .out/kubeconfig

build:
	docker build . -t quay.io/ellorent/ovn-kubevirt
push: build
	docker push quay.io/ellorent/ovn-kubevirt
run: 
	hack/kind.sh
apply:
	kubectl apply -f ovn-kubevirt.yaml

delete: 
	kubectl delete -f ovn-kubevirt.yaml

sync: delete apply
logs:
	kubectl logs -l app=ovn-kubevirt --all-containers --tail=100000
