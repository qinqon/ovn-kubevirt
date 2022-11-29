#!/bin/bash -e
container_name=nb-ovsdb
container_id=$(crictl ps --name $container_name |grep -v CONTAINER |awk '{ print $1 }')
crictl exec $container_id ovs-vsctl $@
