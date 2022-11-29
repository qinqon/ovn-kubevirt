#!/usr/bin/env bash

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

set -euxo pipefail

ROOT_DIR=${DIR}/../
OUTPUT_DIR=${ROOT_DIR}/.out
CNI_DIR=/opt/cni/bin
PLUGIN_NAME=ovn-kubevirt
KIND_CLUSTER_NAME=ovn-kubevirt
KIND_IMAGE=${KIND_IMAGE:-kindest/node}
K8S_VERSION=${K8S_VERSION:-v1.24.0}
CALICO_VERSION=${CALICO_VERSION:-v3.24.5}
CNAO_VERSION=${CNAO_VERSION:-v0.78.0}
KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind-config.yaml}
KUBERNETES_NMSTATE_VERSION=${KUBERNETES_NMSTATE_VERSION:-v0.74.0}
export KUBECONFIG=$OUTPUT_DIR/kubeconfig

function install-kubevirt() {
    KV_VER=$(curl "https://api.github.com/repos/kubevirt/kubevirt/releases/latest" | jq -r ".tag_name")

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-operator.yaml"

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-cr.yaml"
    kubectl wait -n kubevirt kv kubevirt --for=condition=Available --timeout=10m
}

function install-calico() {
    kubectl create -f  https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml
    kubectl rollout status ds/calico-node -n kube-system --timeout=2m
}

function install-ovn-kubevirt() {
    kubectl apply -f $DIR/../ovn-kubevirt.yaml
    kubectl rollout status deployment/ovn-kubevirt
}

function install-network-operators() {
    kubectl apply -f https://github.com/kubevirt/cluster-network-addons-operator/releases/download/${CNAO_VERSION}/namespace.yaml
    kubectl apply -f https://github.com/kubevirt/cluster-network-addons-operator/releases/download/${CNAO_VERSION}/network-addons-config.crd.yaml
    kubectl apply -f https://github.com/kubevirt/cluster-network-addons-operator/releases/download/${CNAO_VERSION}/operator.yaml
    
    cat <<EOF | kubectl apply -f -
---
apiVersion: networkaddonsoperator.network.kubevirt.io/v1
kind: NetworkAddonsConfig
metadata:
  name: cluster
spec:
  imagePullPolicy: IfNotPresent
  multus: {}
  ovs: {}
EOF
    kubectl wait networkaddonsconfig cluster --for condition=Available --timeout=2m
}


function build-cni-plugin() {
    (
        cd ${ROOT_DIR}        
        go build -o ${OUTPUT_DIR}/${PLUGIN_NAME} ./cmd
	    chmod 755 ${OUTPUT_DIR}/${PLUGIN_NAME}
    )
}

function install-cni-plugin(){
    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name")   
    do
        docker cp ${OUTPUT_DIR}/${PLUGIN_NAME}  ${node}:${CNI_DIR}
        docker cp hack/ovs-vsctl.sh  ${node}:/usr/local/bin/ovs-vsctl
    done
}

function run() {
    mkdir -p $OUTPUT_DIR
    if kind get clusters | grep "${KIND_CLUSTER_NAME}"; then
      kind delete cluster --name "${KIND_CLUSTER_NAME}"
    fi
    kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config=${KIND_CONFIG} --retain

    install-calico
    install-ovn-kubevirt
    install-network-operators
    install-kubevirt
}

$1
