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
export CAPK_RELEASE_VERSION="v0.1.0-rc.0"
export KIND_IMAGE=${KIND_IMAGE:-quay.io/ellorent/kindest-node@sha256}
export K8S_VERSION=${K8S_VERSION:-b79f78e961a23c05b5eb8a1c9e9a7b0d9f656ddec078e03f4ca24b1272791c52}

CLUSTERCTL_PATH=$OUTPUT_DIR/clusterctl

function install-kubevirt-release() {
    KV_VER=$(curl "https://api.github.com/repos/kubevirt/kubevirt/releases/latest" | jq -r ".tag_name")

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-operator.yaml"

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-cr.yaml"
}

function install-kubevirt() {
    if ! ls ovn-kubernetes ; then 
        git clone https://github.com/qinqon/kubevirt -b dnm-hypershift-kubevirt
    fi
    pushd kubevirt
        export FEATURE_GATES=KubevirtSeccompProfile
        make DOCKER_PREFIX=localhost:5000/kubevirt cluster-sync
    popd
    sudo sysctl -w vm.unprivileged_userfaultfd=1
    kubectl apply -f hack/allow-post-copy-migration.yaml
}

function wait-kubevirt() {
    kubectl wait -n kubevirt kv kubevirt --for=condition=Available --timeout=10m
}

function install-capk() {
    kubectl apply -f hack/capk.yaml
}

function wait-capk() {
    kubectl wait -n capk-system --for=condition=Available=true deployment/capk-controller-manager --timeout=10m
}

function install-capi() {
    if [ ! -f "${CLUSTERCTL_PATH}" ]; then                                          
        curl -L https://github.com/kubernetes-sigs/cluster-api/releases/download/v1.0.0/clusterctl-linux-amd64 -o ${CLUSTERCTL_PATH}
        chmod u+x ${CLUSTERCTL_PATH}                                                
    fi        
    cat << EOF > ${OUTPUT_DIR}/clusterctl_config.yaml
---
cert-manager:
  url: "https://github.com/cert-manager/cert-manager/releases/latest/cert-manager.yaml"
EOF
    $CLUSTERCTL_PATH init -v 4 --config=${OUTPUT_DIR}/clusterctl_config.yaml
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
    kubectl get vm -n ${TENANT_CLUSTER_NAMESPACE} --no-headers -o custom-columns=":metadata.name" | grep -q $vm_name
}


function create-capk-cluster() {
	
    local cluster_name=cluster1
    local kubernetes_version="v1.23.10"
    export NODE_VM_IMAGE_TEMPLATE=quay.io/ellorent/ubuntu-2004-container-disk:${kubernetes_version}
	export CRI_PATH="/var/run/containerd/containerd.sock"

    kubectl create namespace ${cluster_name} || true

    $CLUSTERCTL_PATH generate cluster ${cluster_name} --target-namespace ${cluster_name} --kubernetes-version  ${kubernetes_version} --control-plane-machine-count=1 --worker-machine-count=1 --from templates/cluster-template.yaml | kubectl apply -f -

	kubectl wait cluster -n $cluster_name $cluster_name --for=condition=Ready --timeout=5m

	retry-until-success kubectl get pods -n kube-system
	retry-until-success vm-matches ${cluster_name} "${cluster_name}-md-"
}

function destroy-capk-cluster() {
    local cluster_name=cluster1
	kubectl delete cluster -n $cluster_name $cluster_name --ignore-not-found
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

function deploy-kubevirt() {
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
}

$1
