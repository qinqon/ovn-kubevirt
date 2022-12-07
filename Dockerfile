FROM golang:1.18 as build

WORKDIR /workspace

COPY go.mod .
COPY go.sum .
RUN go mod download

RUN --mount=type=cache,target=/root/.cache/go-build go install github.com/ovn-org/ovn-kubernetes/go-controller/cmd/ovn-kube-util

FROM quay.io/fedora/fedora:37

USER root

RUN dnf install -y util-linux ovn NetworkManager NetworkManager-ovs ovn-central ovn-host openvswitch && \
    dnf clean all

COPY *.sh ./
COPY --from=build /go/bin/ovn-kube-util /usr/local/bin
RUN touch /etc/default/openvswitch
