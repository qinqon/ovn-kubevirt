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
export K8S_VERSION=${K8S_VERSION:-v1.24.7}
export CAPK_RELEASE_VERSION="v0.1.0-rc.0"

CLUSTERCTL_PATH=$OUTPUT_DIR/clusterctl

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
    kubectl rollout status ds/calico-node -n kube-system --timeout=5m
}

function install-network-manager() {
    kubectl apply -f network-manager.yaml
    kubectl rollout status ds/network-manager
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
  kubeMacPool: {}
EOF
}

function wait-network-operators() {
    kubectl wait networkaddonsconfig cluster --for condition=Available --timeout=5m
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
        kubectl config --kubeconfig=.out/kubeconfig-internal set-cluster ovn-kubevirt --server=https://$(kubectl get svc kubernetes -o json |jq -r .spec.clusterIP)
        docker cp .out/kubeconfig-internal ${node}:/etc/cni/net.d/ovn-kubevirt-kubeconfig
    done
}

function install-cnv() {
    install-network-operators
    install-kubevirt
    wait-network-operators
    wait-kubevirt
    #install-kubernetes-nmstate
    #wait-kubernetes-nmstate
}

function start-kind() {
    if ! ls ovn-kubernetes ; then 
        git clone https://github.com/qinqon/ovn-kubernetes -b ovn-kubevirt
    fi
    pushd ovn-kubernetes
        #pushd go-controller
        #make
        #popd

        #pushd dist/images
        #make fedora
        #popd

        pushd contrib
        ./kind.sh --cluster-name ovn-kubevirt
        popd
    popd 
}

function deploy() {
    pushd ovn-kubernetes/contrib
        ./kind.sh --cluster-name ovn-kubevirt --deploy
    popd
}

function run() {
    start-kind
    for node in $(kubectl get node --no-headers  -o custom-columns=":metadata.name"); do   
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_watches=1048576' >> /etc/sysctl.conf"
        docker exec -t $node bash -c "echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.conf"
        docker exec -i $node bash -c "sysctl -p /etc/sysctl.conf"                      
        docker exec "$node" sysctl --ignore net.ipv6.conf.all.disable_ipv6=0
        docker exec "$node" sysctl --ignore net.ipv6.conf.all.forwarding=1
        if [[ "${node}" =~ worker ]]; then
            kubectl label nodes $node node-role.kubernetes.io/worker="" --overwrite=true
        fi
        docker exec $node apt-get update 
        docker exec $node apt-get install -y tcpdump
    done             
    
    install-cnv
}

$1
