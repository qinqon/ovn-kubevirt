#!/usr/bin/env bash

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

set -euxo pipefail

ROOT_DIR=${DIR}/../
OUTPUT_DIR=${ROOT_DIR}/.out
PLUGIN_NAME=ovn-kubevirt
KIND_CLUSTER_NAME=ovn-kubevirt
KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind-config.yaml}
KUBERNETES_NMSTATE_VERSION=${KUBERNETES_NMSTATE_VERSION:-v0.74.0}
export KUBEVIRT_PROVIDER=external
export KUBECONFIG=$OUTPUT_DIR/kubeconfig
export CAPK_RELEASE_VERSION="v0.1.6"
export KIND_IMAGE=${KIND_IMAGE:-quay.io/ellorent/kindest-node@sha256}
export K8S_VERSION=${K8S_VERSION:-b79f78e961a23c05b5eb8a1c9e9a7b0d9f656ddec078e03f4ca24b1272791c52}
export NODE_VM_IMAGE_TEMPLATE=quay.io/capk/ubuntu-2004-container-disk:v1.24.9
export METALLB_VERSION="v0.13.9"
export CALICO_VERSION="v3.21"

CLUSTERCTL_PATH=$OUTPUT_DIR/clusterctl

function install-kubevirt-release() {
    KV_VER=$(curl "https://api.github.com/repos/kubevirt/kubevirt/releases/latest" | jq -r ".tag_name")

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-operator.yaml"

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-cr.yaml"
}

function install-kubevirt() {
    if ! ls ovn-kubernetes ; then 
        git clone https://github.com/davidvossel/kubevirt -b migration-flip-detection-v2
    fi
    pushd kubevirt
        make DOCKER_PREFIX=localhost:5000/kubevirt cluster-sync
    popd
    #sudo sysctl -w vm.unprivileged_userfaultfd=1
    #kubectl apply -f hack/allow-post-copy-migration.yaml
}

function wait-kubevirt() {
    kubectl wait -n kubevirt kv kubevirt --for=condition=Available --timeout=10m
}

function install-capk() {
    clusterctl init -v 4 --config=hack/clusterctl_config.yaml    
    kubectl apply -f https://github.com/kubernetes-sigs/cluster-api-provider-kubevirt/releases/download/${CAPK_RELEASE_VERSION}/infrastructure-components.yaml
    kubectl wait -n capk-system --for=condition=Available=true deployment/capk-controller-manager --timeout=10m
}

function install-metallb {
    kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/${METALLB_VERSION}/config/manifests/metallb-native.yaml
	kubectl -n metallb-system wait deployment controller --for condition=Available --timeout=10m

	cat << EOF | kubectl apply -f  -
---
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: ovn-kubevirt
  namespace: metallb-system
spec:
  addresses:
  - 172.18.0.201-172.18.0.250
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: ovn-kubevirt
  namespace: metallb-system
EOF

}

function retry-until-success {
    local timeout=30
    local interval=1
    until $@; do
        ((timeout--)) && ((timeout==0)) && echo "condition not met" && exit 1
        echo "waiting for \"$@\""
        sleep $interval 
    done
}

function vm-matches {
    local vm_namespace=$1
    local vm_name=$2
    kubectl get vm -n cluster1 --no-headers -o custom-columns=":metadata.name" | grep -q $vm_name
}

function create-tenant-cluster() {
	export CRI_PATH="/var/run/containerd/containerd.sock"
	kubectl create namespace cluster1 || true
    clusterctl generate cluster cluster1 --target-namespace cluster1 --kubernetes-version v1.24.9 --control-plane-machine-count=1 --worker-machine-count=1 --from hack/cluster-template.yaml | kubectl apply -f -
	
    kubectl wait cluster -n cluster1 cluster1 --for=condition=Ready --timeout=10m
    clusterctl get kubeconfig -n cluster1 cluster1 > cluster1-kubeconfig
    KUBECONFIG=cluster1-kubeconfig retry-until-success kubectl get pods -n kube-system
	retry-until-success vm-matches cluster1 "cluster1-md-"
    kubectl wait vmi -n cluster1 --for=condition=Ready $(kubectl get --no-headers vmi -n cluster1 -l cluster.x-k8s.io/cluster-name=cluster1 -l cluster.x-k8s.io/role=worker |awk '{print $1}') --timeout=2m
    
    KUBECONFIG=cluster1-kubeconfig kubectl apply -f https://docs.projectcalico.org/${CALICO_VERSION}/manifests/calico.yaml
	KUBECONFIG=cluster1-kubeconfig kubectl rollout status ds/calico-node -n kube-system --timeout=2m
}

function destroy-tenant-cluster() {
    kubectl delete cluster -n cluster1 cluster1 --ignore-not-found
}

function install-cnv() {
    #install-network-operators
    install-kubevirt
    #wait-network-operators
    wait-kubevirt
    #install-kubernetes-nmstate
    #wait-kubernetes-nmstate
}

function start-kind() {
    if ! ls ovn-kubernetes ; then 
        git clone https://github.com/qinqon/ovn-kubernetes -b spike-kubevirt-migration
    fi

    # create registry container unless it already exists
    reg_name='kind-registry'
    reg_port='5000'
    if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != 'true' ]; then
      docker run \
        -d --restart=always -p "127.0.0.1:${reg_port}:5000" --name "${reg_name}" \
        registry:2
    fi
    
    # connect the registry to the cluster network if not already connected
    if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
        docker network connect "kind" "${reg_name}"
    fi

    pushd ovn-kubernetes
        pushd contrib
        ./kind.sh --local-kind-registry --cluster-name ovn-kubevirt
        popd
    popd 

}

function deploy() {
    pushd ovn-kubernetes/contrib
        ./kind.sh --local-kind-registry --cluster-name ovn-kubevirt --deploy
    popd
}

function patch-kubevirt() {
    pushd kubevirt
        make DOCKER_PREFIX=localhost:5000/kubevirt cluster-patch
    popd
}

function run() {
    start-kind
    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name"); do   
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_watches=1048576' >> /etc/sysctl.conf"
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.conf"
        docker exec -i $node bash -c "sysctl -p /etc/sysctl.conf"                      
        #docker exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
        #docker exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1
        if [[ "${node}" =~ worker ]]; then
            kubectl label nodes $node node-role.kubernetes.io/worker="" --overwrite=true
        fi
        #docker exec $node apt-get update 
        #docker exec $node apt-get install -y tcpdump
    done             
    
    install-cnv
    install-metallb
}

function demo() {
    install-capk
    create-tenant-cluster
}

$1
