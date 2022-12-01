// Copyright 2017 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a sample chained plugin that supports multiple CNI versions. It
// parses prevResult according to the cniVersion
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

type PluginConf struct {
	types.NetConf
	Router     string `json:"router"`
	LeaseTime  string `json:"lease-time"`
	Subnet     string `json:"subnet"`
	ExcludeIps string `json:"exclude-ips"`
}

type ExtraArgs struct {
	K8S_POD_NAMESPACE, K8S_POD_NAME cnitypes.UnmarshallableString
	cnitypes.CommonArgs
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result. This will parse, validate, and place the
	// previous result object into conf.PrevResult. If you need to modify
	// or inspect the PrevResult you will need to convert it to a concrete
	// versioned Result struct.
	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	return &conf, nil
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	logCall("ADD", args)
	cniConf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	// A plugin can be either an "originating" plugin or a "chained" plugin.
	// Originating plugins perform initial sandbox setup and do not require
	// any result from a previous plugin in the chain. A chained plugin
	// modifies sandbox configuration that was previously set up by an
	// originating plugin and may optionally require a PrevResult from
	// earlier plugins in the chain.

	// START chained plugin code
	if cniConf.PrevResult == nil {
		return fmt.Errorf("must be called chained with ovs plugin")
	}

	// Convert the PrevResult to a concrete Result type that can be modified.
	prevResult, err := current.GetResult(cniConf.PrevResult)
	if err != nil {
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	portName, err := composePortNameFromExtraArgs(args.Args)
	if err != nil {
		return err
	}
	setIfaceIDCmd := exec.Command("ovs-vsctl", "add", "Interface", prevResult.Interfaces[0].Name, "external_ids", fmt.Sprintf("iface-id=%s", portName))
	setIfaceIDCmd.Env = []string{"KUBECONFIG=/etc/kubernetes/admin.conf"}
	if output, err := setIfaceIDCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %v", output, err)
	}

	cli, err := newClient()
	if err != nil {
		return err
	}

	err = cli.Connect(context.Background())
	if err != nil {
		return err
	}

	//TODO: Retrieve the VMI mac address
	vmiMAC := "1a:3c:0f:bf:92:50"

	dhcpOptions := nbdb.DHCPOptions{
		Cidr: cniConf.Subnet,
		Options: map[string]string{
			"lease_time": cniConf.LeaseTime,
			"router":     cniConf.Router,
			"server_id":  cniConf.Router,
			"server_mac": "c0:ff:ee:00:00:01",
		},
	}

	ops, err := cli.Create(&dhcpOptions)
	if err != nil {
		return fmt.Errorf("failed creating dhcp options: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(cli, ops)
	if err != nil {
		return fmt.Errorf("failed commiting dhcp options: %v", err)
	}

	dhcpOptionsResult := []nbdb.DHCPOptions{}
	if err := cli.List(context.Background(), &dhcpOptionsResult); err != nil {
		return fmt.Errorf("failed listing dhcp options: %v", err)
	}
	if len(dhcpOptionsResult) == 0 {
		return fmt.Errorf("missing dhcp options")
	}

	ls := nbdb.LogicalSwitch{
		Name: cniConf.Name,
		OtherConfig: map[string]string{
			"subnet":      cniConf.Subnet,
			"exclude_ips": cniConf.ExcludeIps,
		},
	}

	enabled := true
	lsp := nbdb.LogicalSwitchPort{
		Name:          portName,
		Addresses:     []string{vmiMAC, "dynamic"},
		Enabled:       &enabled,
		Dhcpv4Options: &dhcpOptionsResult[0].UUID,
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(cli, &ls, &lsp); err != nil {
		return err
	}

	return types.PrintResult(&current.Result{}, cniConf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	logCall("DEL", args)
	cniConf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	cli, err := newClient()
	if err != nil {
		return err
	}

	portName, err := composePortNameFromExtraArgs(args.Args)
	if err != nil {
		return err
	}

	if err := libovsdbops.DeleteLogicalSwitchPorts(cli, &nbdb.LogicalSwitch{Name: cniConf.Name}, &nbdb.LogicalSwitchPort{Name: portName}); err != nil {
		return err
	}

	if err := libovsdbops.DeleteLogicalSwitch(cli, cniConf.Name); err != nil {
		return err
	}
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("OVN kubevirt"))
}

func newClient() (client.Client, error) {
	ovsNbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	cli, err := client.NewOVSDBClient(ovsNbModel, client.WithEndpoint("tcp:172.18.0.2:6641"))

	err = cli.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err := cli.MonitorAll(context.Background()); err != nil {
		return nil, err
	}
	return cli, nil
}

func composePortNameFromExtraArgs(args string) (string, error) {
	extraArgs, err := parseArgs(args)
	if err != nil {
		return "", err
	}
	if extraArgs.K8S_POD_NAMESPACE == "" {
		return "", fmt.Errorf("missing K8S_POD_NAMESPACE")
	}
	if extraArgs.K8S_POD_NAME == "" {
		return "", fmt.Errorf("missing K8S_POD_NAME")
	}

	return composePortName(string(extraArgs.K8S_POD_NAMESPACE), string(extraArgs.K8S_POD_NAME)), nil
}

func composePortName(podNamespace, podName string) string {
	return podNamespace + "_" + podName
}

func logCall(command string, args *skel.CmdArgs) {
	log.Printf("CNI %s was called for container ID: %s, network namespace %s, interface name %s, configuration: %s, args: %s",
		command, args.ContainerID, args.Netns, args.IfName, string(args.StdinData[:]), args.Args)
}

func parseArgs(envArgsString string) (*ExtraArgs, error) {
	if envArgsString != "" {
		e := ExtraArgs{}
		err := cnitypes.LoadArgs(envArgsString, &e)
		if err != nil {
			return nil, err
		}
		return &e, nil
	}
	return nil, nil
}
