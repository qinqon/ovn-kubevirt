#!/usr/bin/env bash

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

set -euxo pipefail

OUTPUT_DIR=${DIR}/../.out
KIND_CLUSTER_NAME=ovn-kubevirt
KIND_IMAGE=${KIND_IMAGE:-kindest/node}
K8S_VERSION=${K8S_VERSION:-v1.24.0}
CALICO_VERSION=${CALICO_VERSION:-v3.24.5}
KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind-config.yaml}
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

mkdir -p $OUTPUT_DIR
if kind get clusters | grep "${KIND_CLUSTER_NAME}"; then
  kind delete cluster --name "${KIND_CLUSTER_NAME}"
fi
kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config=${KIND_CONFIG} --retain

install-calico
install-ovn-kubevirt
#install-kubevirt
