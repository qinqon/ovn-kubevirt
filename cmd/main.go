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
	"os"
	"os/exec"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"

	kubevirtv1 "kubevirt.io/api/core/v1"

	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	ovsclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

var (
	pluginscheme = runtime.NewScheme()
)

func init() {
	if err := scheme.AddToScheme(pluginscheme); err != nil {
		panic(err)
	}
	if err := kubevirtv1.AddToScheme(pluginscheme); err != nil {
		panic(err)
	}
}

type PluginConf struct {
	types.NetConf
	Router     string `json:"router"`
	LeaseTime  string `json:"lease-time"`
	Subnet     string `json:"subnet"`
	ExcludeIps string `json:"exclude-ips"`
}

type ExtraArgs struct {
	MAC, K8S_POD_NAMESPACE, K8S_POD_NAME cnitypes.UnmarshallableString
	cnitypes.CommonArgs
}

type CmdContext struct {
	k8scli       k8sclient.Client
	ovscli       ovsclient.Client
	conf         *PluginConf
	mac          string
	vmi          *kubevirtv1.VirtualMachineInstance
	virtLauncher *corev1.Pod
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
	ctx, err := loadCmdContext(args)
	if err != nil {
		return err
	}

	if ctx.conf.PrevResult == nil {
		return fmt.Errorf("must be called chained with ovs plugin")
	}

	prevResult, err := current.GetResult(ctx.conf.PrevResult)
	if err != nil {
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	portName := composePortName(ctx.vmi.Namespace, ctx.vmi.Name)
	output, err := runOVSVsctl("add", "Interface", prevResult.Interfaces[0].Name, "external_ids", fmt.Sprintf("iface-id=%s", portName))
	if err != nil {
		return fmt.Errorf("%s: %v", output, err)
	}

	dhcpOptions := nbdb.DHCPOptions{
		Cidr: ctx.conf.Subnet,
		Options: map[string]string{
			"lease_time": ctx.conf.LeaseTime,
			"router":     ctx.conf.Router,
			"server_id":  ctx.conf.Router,
			"server_mac": "c0:ff:ee:00:00:01",
		},
	}

	ops, err := ctx.ovscli.Create(&dhcpOptions)
	if err != nil {
		return fmt.Errorf("failed creating dhcp options: %v", err)
	}
	_, err = libovsdbops.TransactAndCheck(ctx.ovscli, ops)
	if err != nil {
		return fmt.Errorf("failed commiting dhcp options: %v", err)
	}

	dhcpOptionsResult := []nbdb.DHCPOptions{}
	if err := ctx.ovscli.List(context.Background(), &dhcpOptionsResult); err != nil {
		return fmt.Errorf("failed listing dhcp options: %v", err)
	}
	if len(dhcpOptionsResult) == 0 {
		return fmt.Errorf("missing dhcp options")
	}

	ls := nbdb.LogicalSwitch{
		Name: ctx.conf.Name,
		OtherConfig: map[string]string{
			"subnet":      ctx.conf.Subnet,
			"exclude_ips": ctx.conf.ExcludeIps,
		},
	}

	address := "dynamic"
	// virt-launcher pod has the mac on the annotation
	if ctx.mac != "" {
		address = ctx.mac + " " + address
	}

	enabled := true
	lsp := nbdb.LogicalSwitchPort{
		Name:          portName,
		Addresses:     []string{address},
		Enabled:       &enabled,
		Dhcpv4Options: &dhcpOptionsResult[0].UUID,
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(ctx.ovscli, &ls, &lsp); err != nil {
		return err
	}

	return types.PrintResult(&current.Result{}, ctx.conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	logCall("DEL", args)
	ctx, err := loadCmdContext(args)
	if err != nil {
		return err
	}

	// If this is the origin virt-launcher pod of a live migrated vmi
	// don't remove the logical switch, it's being use by the target
	// virt-launcher pod
	if ctx.vmi.Status.MigrationState.TargetPod != ctx.virtLauncher.Name {
		return nil
	}

	portName := composePortName(ctx.vmi.Namespace, ctx.vmi.Name)
	if err := libovsdbops.DeleteLogicalSwitchPorts(ctx.ovscli, &nbdb.LogicalSwitch{Name: ctx.conf.Name}, &nbdb.LogicalSwitchPort{Name: portName}); err != nil {
		return err
	}

	//FIXME: Switch has to be delete on "tenant" removal
	/*
		if err := libovsdbops.DeleteLogicalSwitch(cli, ctx.conf.Name); err != nil {
			return err
		}
	*/
	return nil
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("OVN kubevirt"))
}

func newNBOVNClient() (ovsclient.Client, error) {
	ovsNbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	cli, err := ovsclient.NewOVSDBClient(ovsNbModel, ovsclient.WithEndpoint("tcp:ovn-kubevirt-control-plane:6641"))

	err = cli.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	if _, err := cli.MonitorAll(context.Background()); err != nil {
		return nil, err
	}
	return cli, nil
}

func newK8SClient() (k8sclient.Client, error) {
	kubeConfig, err := os.ReadFile("/etc/cni/net.d/ovn-kubevirt-kubeconfig")
	if err != nil {
		return nil, err
	}
	restCfg, err := clientcmd.RESTConfigFromKubeConfig(kubeConfig)
	if err != nil {
		return nil, err
	}

	return k8sclient.New(restCfg, k8sclient.Options{Scheme: pluginscheme})
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

func runOVSVsctl(args ...string) ([]byte, error) {
	kubeconfigEnv := []string{"KUBECONFIG=/etc/cni/net.d/ovn-kubevirt-kubeconfig"}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	//TODO: Use k8s clientset
	cmd := exec.Command("kubectl", "get", "pod", "-l", "app=ovn-kubevirt-node", "--no-headers", "-o", "name", "--field-selector", fmt.Sprintf("spec.nodeName=%s", hostname))
	cmd.Env = kubeconfigEnv
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, err
	}
	podName := strings.TrimSuffix(string(output), "\n")
	cmd = exec.Command("kubectl", append([]string{"exec", podName, "-c", "ovs-server", "--", "ovs-vsctl"}, args...)...)
	cmd.Env = kubeconfigEnv
	return cmd.CombinedOutput()
}

func loadCmdContext(args *skel.CmdArgs) (*CmdContext, error) {
	ctx := CmdContext{}

	var err error
	ctx.k8scli, err = newK8SClient()
	if err != nil {
		return nil, err
	}
	ctx.ovscli, err = newNBOVNClient()
	if err != nil {
		return nil, err
	}

	ctx.conf, err = parseConfig(args.StdinData)
	if err != nil {
		return nil, err
	}

	extraArgs, err := parseArgs(args.Args)
	if err != nil {
		return nil, err
	}

	ctx.mac = string(extraArgs.MAC)
	if extraArgs.K8S_POD_NAMESPACE == "" {
		return nil, fmt.Errorf("missing K8S_POD_NAMESPACE")
	}

	if extraArgs.K8S_POD_NAME == "" {
		return nil, fmt.Errorf("missing K8S_POD_NAME")
	}

	ctx.virtLauncher = &corev1.Pod{}
	if err := ctx.k8scli.Get(context.Background(), k8sclient.ObjectKey{Namespace: string(extraArgs.K8S_POD_NAMESPACE), Name: string(extraArgs.K8S_POD_NAME)}, ctx.virtLauncher); err != nil {
		return nil, err
	}

	if ctx.virtLauncher.Labels == nil {
		return nil, fmt.Errorf("missing virt-launcher labels")
	}
	vmName, ok := ctx.virtLauncher.Labels["vm.kubevirt.io/name"]
	if !ok {
		return nil, fmt.Errorf("missing virt-launcher label vm.kubevirt.io/name")
	}

	ctx.vmi = &kubevirtv1.VirtualMachineInstance{}
	if err := ctx.k8scli.Get(context.Background(), k8sclient.ObjectKey{Namespace: ctx.virtLauncher.Namespace, Name: vmName}, ctx.vmi); err != nil {
		return nil, err
	}
	return &ctx, nil
}
