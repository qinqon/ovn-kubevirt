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
        docker cp hack/ovs-vsctl  ${node}:/usr/local/bin
    done
}

function run() {
    mkdir -p $OUTPUT_DIR
    if kind get clusters | grep "${KIND_CLUSTER_NAME}"; then
      kind delete cluster --name "${KIND_CLUSTER_NAME}"
    fi

    # create registry container unless it already exists
    reg_name='kind-registry'
    reg_port='5001'
    if [ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != 'true' ]; then
      docker run \
        -d --restart=always -p "127.0.0.1:${reg_port}:5000" --name "${reg_name}" \
        registry:2
    fi

    cat <<EOF | kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}":"${K8S_VERSION}" --config=- --retain
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
# the default CNI will not be installed
  disableDefaultCNI: true
nodes:
- role: control-plane
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:${reg_port}"]
    endpoint = ["http://${reg_name}:5000"]
EOF
    
    # connect the registry to the cluster network if not already connected
    if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "${reg_name}")" = 'null' ]; then
      docker network connect "kind" "${reg_name}"
    fi

    # Document the local registry
    # https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-a-local-registry
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${reg_port}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

    install-calico
    install-ovn-kubevirt
    install-network-operators
    install-kubevirt
}

$1
