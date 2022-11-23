FROM quay.io/fedora/fedora:37


RUN dnf install -y util-linux ovn ovn-central ovn-host openvswitch && \
    dnf clean all

COPY *.sh /
