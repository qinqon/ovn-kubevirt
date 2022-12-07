#!/usr/bin/env bash

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

set -euxo pipefail

ROOT_DIR=${DIR}/../
OUTPUT_DIR=${ROOT_DIR}/.out
CNI_DIR=/opt/cni/bin
PLUGIN_NAME=ovn-kubevirt
KIND_CLUSTER_NAME=ovn-kubevirt
KIND_IMAGE=${KIND_IMAGE:-kindest/node:v1.24.7@sha256:577c630ce8e509131eab1aea12c022190978dd2f745aac5eb1fe65c0807eb315}
CALICO_VERSION=${CALICO_VERSION:-v3.24.5}
CNAO_VERSION=${CNAO_VERSION:-v0.78.0}
KIND_CONFIG=${KIND_CONFIG:-${DIR}/kind-config.yaml}
KUBERNETES_NMSTATE_VERSION=${KUBERNETES_NMSTATE_VERSION:-v0.74.0}
export KUBECONFIG=$OUTPUT_DIR/kubeconfig

function install-kubevirt() {
    KV_VER=$(curl "https://api.github.com/repos/kubevirt/kubevirt/releases/latest" | jq -r ".tag_name")

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-operator.yaml"

    kubectl apply -f "https://github.com/kubevirt/kubevirt/releases/download/${KV_VER}/kubevirt-cr.yaml"
}

function wait-kubevirt() {
    kubectl wait -n kubevirt kv kubevirt --for=condition=Available --timeout=10m
}

function install-calico() {
    kubectl create -f  https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml
    kubectl rollout status ds/calico-node -n kube-system --timeout=2m
}

function install-network-manager() {
    kubectl apply -f network-manager.yaml
    kubectl rollout status ds/network-manager
}


function install-ovn-kubevirt() {
    kubectl apply -f $DIR/../ovn-kubevirt.yaml
}
function wait-ovn-kubevirt() {
    kubectl rollout status deployment/ovn-kubevirt-control-plane
    kubectl rollout status ds/ovn-kubevirt-node
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
}

function wait-network-operators() {
    kubectl wait networkaddonsconfig cluster --for condition=Available --timeout=2m
}

function install-kubernetes-nmstate() {
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/nmstate.io_nmstates.yaml
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/namespace.yaml
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/service_account.yaml
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/role.yaml
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/role_binding.yaml
    kubectl apply -f https://github.com/nmstate/kubernetes-nmstate/releases/download/${KUBERNETES_NMSTATE_VERSION}/operator.yaml
    cat <<EOF | kubectl create -f -
apiVersion: nmstate.io/v1
kind: NMState
metadata:
  name: nmstate
EOF
}

function wait-kubernetes-nmstate() {
    kubectl rollout status -w -n nmstate ds nmstate-handler --timeout=2m
    kubectl rollout status -w -n nmstate deployment nmstate-webhook --timeout=2m
}

function build-cni-plugin() {
    (
        cd ${ROOT_DIR}        
        go build -o ${OUTPUT_DIR}/${PLUGIN_NAME} ./cmd/plugin
	    chmod 755 ${OUTPUT_DIR}/${PLUGIN_NAME}
    )
}

function install-cni-plugin(){
    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name")   
    do
        docker cp ${OUTPUT_DIR}/${PLUGIN_NAME}  ${node}:${CNI_DIR}
        docker cp hack/ovs-vsctl  ${node}:/usr/local/bin
        cp .out/kubeconfig .out/kubeconfig-internal
        kubectl config --kubeconfig=.out/kubeconfig-internal set-cluster kind-ovn-kubevirt --server=https://$(kubectl get svc kubernetes -o json |jq -r .spec.clusterIP)
        docker cp .out/kubeconfig-internal ${node}:/etc/cni/net.d/ovn-kubevirt-kubeconfig
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

    cat <<EOF | kind create cluster --name "${KIND_CLUSTER_NAME}" --kubeconfig "${KUBECONFIG}" --image "${KIND_IMAGE}" --config=- --retain
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  ipFamily:
# the default CNI will not be installed
#  disableDefaultCNI: true
nodes:
- role: control-plane
- role: worker
- role: worker
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
    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name"); do   
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_watches=1048576' >> /etc/sysctl.conf"
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.conf"
        docker exec -i $node bash -c "sysctl -p /etc/sysctl.conf"                      
        docker exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
        docker exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1
        if [[ "${node}" =~ worker ]]; then
            kubectl label nodes $node node-role.kubernetes.io/worker="" --overwrite=true
        fi
    done             
    #install-calico
    #install-network-manager

    install-ovn-kubevirt
    install-network-operators
    install-kubevirt
    #install-kubernetes-nmstate
    wait-ovn-kubevirt
    wait-network-operators
    wait-kubevirt
    #wait-kubernetes-nmstate
}

$1
