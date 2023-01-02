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
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"

	kubevirtv1 "kubevirt.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"

	ovsclient "github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	ovnktypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

var (
	pluginscheme = runtime.NewScheme()
	enabled      = true
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
	nbcli        ovsclient.Client
	sbcli        ovsclient.Client
	conf         *PluginConf
	mac          string
	vmi          *kubevirtv1.VirtualMachineInstance
	virtLauncher *corev1.Pod
	gateway      *Gateway
	joinRouter   *JoinRouter
	hostname     string
}

type GatewayRouter struct {
	lr       *nbdb.LogicalRouter
	gwPort   *nbdb.LogicalRouterPort
	joinPort *nbdb.LogicalRouterPort
}

type Gateway struct {
	routers map[string]*GatewayRouter
}

type JoinRouter struct {
	lr          *nbdb.LogicalRouter
	gwPorts     map[string]*nbdb.LogicalRouterPort
	tenantPorts map[string]*nbdb.LogicalRouterPort
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
	logCall("FOO", args)
	logCall("ADD", args)
	ctx, err := loadCmdContext(args)
	if err != nil {
		return fmt.Errorf("failed loading cmd config: %v", err)
	}

	if ctx.conf.PrevResult == nil {
		return fmt.Errorf("must be called chained with ovs plugin")
	}

	prevResult, err := current.GetResult(ctx.conf.PrevResult)
	if err != nil {
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	portName := composePortName(ctx.vmi.Namespace, ctx.vmi.Name)
	output, err := runOVSVsctl(ctx, "add", "Interface", prevResult.Interfaces[0].Name, "external_ids", fmt.Sprintf("iface-id=%s", portName))
	if err != nil {
		return fmt.Errorf("%s: %v", output, err)
	}

	ctx.joinRouter = newJoinRouter()
	ctx.joinRouter.addTenantPort(ctx)

	if err := ctx.joinRouter.ensure(ctx); err != nil {
		return fmt.Errorf("failed ensuring join router: %v", err)
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

	dnsServer, err := kubeDNSNameServer(ctx)
	if err != nil {
		return err
	}

	dhcpOptions := nbdb.DHCPOptions{
		Cidr: ctx.conf.Subnet,
		Options: map[string]string{
			"lease_time": ctx.conf.LeaseTime,
			"router":     ctx.conf.Router,
			"dns_server": dnsServer,
			"server_id":  ctx.conf.Router,
			"server_mac": "c0:ff:ee:00:00:01",
		},
	}

	if err := ensureDHCPOptions(ctx, &dhcpOptions); err != nil {
		return err
	}
	lsps := []*nbdb.LogicalSwitchPort{
		&nbdb.LogicalSwitchPort{
			Name:          portName,
			Addresses:     []string{address},
			Enabled:       &enabled,
			Dhcpv4Options: &dhcpOptions.UUID,
		},
		&nbdb.LogicalSwitchPort{
			Name:      ctx.conf.Name + "-to-ovn_cluster_router",
			Type:      "router",
			Addresses: []string{"router"},
			Enabled:   &enabled,
			Options: map[string]string{
				"router-port": ctx.conf.Name,
			},
		},
	}
	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(ctx.nbcli, &ls, lsps...); err != nil {
		return fmt.Errorf("failed ensuring tenant logical switch and ports: %v", err)
	}

	if err := masqueradeTenantSubnet(ctx); err != nil {
		return err
	}

	if err := routeTenantSubnetToJoinRouter(ctx); err != nil {
		return err
	}

	if err := ctx.joinRouter.routeDynamicAddressToGw(ctx, lsps[0]); err != nil {
		return err
	}

	return types.PrintResult(&current.Result{}, ctx.conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	return nil
	logCall("DEL", args)
	ctx, err := loadCmdContext(args)
	if err != nil {
		return err
	}

	// If this is the origin virt-launcher pod of a live migrated vmi
	// don't remove the logical switch, it's being use by the target
	// virt-launcher pod
	if ctx.vmi.Status.MigrationState != nil && ctx.vmi.Status.MigrationState.TargetPod != ctx.virtLauncher.Name {
		return nil
	}

	portName := composePortName(ctx.vmi.Namespace, ctx.vmi.Name)
	if err := libovsdbops.DeleteLogicalSwitchPorts(ctx.nbcli, &nbdb.LogicalSwitch{Name: ctx.conf.Name}, &nbdb.LogicalSwitchPort{Name: portName}); err != nil {
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

func newNBClient(ctx *CmdContext) (ovsclient.Client, error) {
	ovsNbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	return newOVSClient(ctx, ovsNbModel, "6641")
}

func newSBClient(ctx *CmdContext) (ovsclient.Client, error) {
	ovsSbModel, err := sbdb.FullDatabaseModel()
	if err != nil {
		return nil, err
	}

	return newOVSClient(ctx, ovsSbModel, "6642")
}

func newOVSClient(ctx *CmdContext, ovsModel model.ClientDBModel, port string) (ovsclient.Client, error) {
	endpointSliceList := &discoveryv1.EndpointSliceList{}
	if err := ctx.k8scli.List(context.Background(), endpointSliceList,
		client.InNamespace("ovn-kubernetes"),
		client.MatchingLabels(map[string]string{"kubernetes.io/service-name": "ovnkube-db"})); err != nil {
		return nil, err
	}
	if len(endpointSliceList.Items) == 0 {
		return nil, fmt.Errorf("missing ovnkube-db endpoint slice")
	}
	if len(endpointSliceList.Items[0].Endpoints) == 0 {
		return nil, fmt.Errorf("missing ovnkube-db endpoint")
	}
	if len(endpointSliceList.Items[0].Endpoints[0].Addresses) == 0 {
		return nil, fmt.Errorf("missing ovnkube-db address")
	}
	endpoint := fmt.Sprintf("tcp:%s:%s", endpointSliceList.Items[0].Endpoints[0].Addresses[0], port)
	cli, err := ovsclient.NewOVSDBClient(ovsModel, ovsclient.WithEndpoint(endpoint))

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

func runOVSVsctl(ctx *CmdContext, args ...string) (string, error) {
	kubeconfigEnv := []string{"KUBECONFIG=/etc/cni/net.d/ovn-kubevirt-kubeconfig"}
	//TODO: Use k8s clientset
	cmd := exec.Command("kubectl", "get", "pod", "-n", "ovn-kubernetes", "-l", "app=ovs-node", "--no-headers", "-o", "name", "--field-selector", fmt.Sprintf("spec.nodeName=%s", ctx.hostname))
	cmd.Env = kubeconfigEnv
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %v", output, err)
	}
	podName := strings.TrimSuffix(string(output), "\n")
	cmd = exec.Command("kubectl", append([]string{"exec", podName, "-n", "ovn-kubernetes", "-c", "ovs-daemons", "--", "ovs-vsctl"}, args...)...)
	cmd.Env = kubeconfigEnv
	output, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %v", output, err)
	}
	return strings.Trim(strings.TrimSpace(string(output)), "\""), nil
}

func loadCmdContext(args *skel.CmdArgs) (*CmdContext, error) {
	ctx := CmdContext{}

	var err error
	ctx.hostname, err = os.Hostname()
	if err != nil {
		return nil, err
	}

	ctx.k8scli, err = newK8SClient()
	if err != nil {
		return nil, err
	}
	ctx.nbcli, err = newNBClient(&ctx)
	if err != nil {
		return nil, err
	}

	ctx.sbcli, err = newSBClient(&ctx)
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

func ensureDHCPOptions(ctx *CmdContext, dhcpOptions *nbdb.DHCPOptions) error {
	dhcpOptionsResult := []nbdb.DHCPOptions{}
	if err := ctx.nbcli.List(context.Background(), &dhcpOptionsResult); err != nil {
		return fmt.Errorf("failed listing dhcp options: %v", err)
	}
	ops := []ovsdb.Operation{}
	if len(dhcpOptionsResult) == 0 {
		var err error
		ops, err = ctx.nbcli.Create(dhcpOptions)
		if err != nil {
			return fmt.Errorf("failed creating dhcp options: %v", err)
		}
	} else {
		for _, d := range dhcpOptionsResult {
			if d.Cidr == ctx.conf.Subnet {
				var err error
				ops, err = ctx.nbcli.Where(&d).Update(dhcpOptions)
				if err != nil {
					return fmt.Errorf("failed updating dhcp options: %v", err)
				}
				break
			}
		}
	}

	_, err := libovsdbops.TransactAndCheck(ctx.nbcli, ops)
	if err != nil {
		return fmt.Errorf("failed commiting dhcp options: %v", err)
	}

	dhcpOptionsResult = []nbdb.DHCPOptions{}
	if err := ctx.nbcli.List(context.Background(), &dhcpOptionsResult); err != nil {
		return fmt.Errorf("failed listing dhcp options: %v", err)
	}
	if len(dhcpOptionsResult) == 0 {
		return fmt.Errorf("missing dhcp options")
	}
	*dhcpOptions = dhcpOptionsResult[0]
	return nil
}

func kubeDNSNameServer(ctx *CmdContext) (string, error) {
	svc := &corev1.Service{}
	if err := ctx.k8scli.Get(context.Background(), client.ObjectKey{Namespace: "kube-system", Name: "kube-dns"}, svc); err != nil {
		return "", err
	}

	return svc.Spec.ClusterIP, nil
}

func nodeIP(ctx *CmdContext, nodeName string) (string, error) {
	node := &corev1.Node{}
	if err := ctx.k8scli.Get(context.Background(), client.ObjectKey{Name: nodeName}, node); err != nil {
		return "", err
	}
	if len(node.Status.Addresses) == 0 {
		return "", fmt.Errorf("missing address at node %s", nodeName)
	}
	return node.Status.Addresses[0].Address, nil
}

func nodes(ctx *CmdContext) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := ctx.k8scli.List(context.Background(), nodeList); err != nil {
		return nil, err
	}
	return nodeList.Items, nil

}

// of type src-ip [VM IP -> gw router ip] since it has higher priority than the
// router ports subnet, so we need to implement it with policies
func (j *JoinRouter) routeDynamicAddressToGw(ctx *CmdContext, lsp *nbdb.LogicalSwitchPort) error {

	if err := j.ensureDummyRoute(ctx); err != nil {
		return err
	}

	if err := j.ensureKeepInternalTrafficNextHopPolicy(ctx); err != nil {
		return err
	}

	if err := j.ensureRerouteToGwPolicy(ctx, lsp); err != nil {
		return err
	}

	return nil
}

func (j *JoinRouter) ensureDummyRoute(ctx *CmdContext) error {

	// Add a dummy route to match the tenant cluster so we can continue implementing
	// routing with policies (if there is no match policies are not run).
	dummyRoute := nbdb.LogicalRouterStaticRoute{
		IPPrefix: ctx.conf.Subnet,
		Nexthop:  ctx.conf.Router,
		Policy:   &nbdb.LogicalRouterStaticRoutePolicySrcIP,
	}

	p := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Policy != nil && *item.Policy == *dummyRoute.Policy && item.Nexthop == dummyRoute.Nexthop && item.IPPrefix == dummyRoute.IPPrefix
	}
	if err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(ctx.nbcli, j.lr.Name, &dummyRoute, p); err != nil {
		return fmt.Errorf("failed ensuring dummy route: %v", err)
	}
	return nil
}

func (j *JoinRouter) ensureKeepInternalTrafficNextHopPolicy(ctx *CmdContext) error {
	// Add a allow policy with higher priority to keep nexthop for e/s traffic
	// TODO: Read the internal subnets from the system
	podCIDR := "10.244.0.0/16"
	internalCIDR := "100.64.0.0/16"
	policy := nbdb.LogicalRouterPolicy{
		Match:    fmt.Sprintf("ip4.src == %s && ip4.dst == { %s, %s }", ctx.conf.Subnet, podCIDR, internalCIDR),
		Action:   nbdb.LogicalRouterPolicyActionAllow,
		Priority: 2,
	}

	predicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == policy.Priority && item.Match == policy.Match && item.Action == policy.Action
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(ctx.nbcli, j.lr.Name, &policy, predicate); err != nil {
		return fmt.Errorf("failed ensuring policy at cluster router to keep e/w nexthop: %v", err)
	}
	return nil
}

func (j *JoinRouter) ensureRerouteToGwPolicy(ctx *CmdContext, lsp *nbdb.LogicalSwitchPort) error {
	nodeLRP := &nbdb.LogicalRouterPort{
		Name: ovnktypes.GWRouterToJoinSwitchPrefix + ovnktypes.GWRouterPrefix + ctx.hostname,
	}

	nodeLRP, err := libovsdbops.GetLogicalRouterPort(ctx.nbcli, nodeLRP)
	if err != nil {
		return err
	}
	nodeLRPIP, _, err := net.ParseCIDR(nodeLRP.Networks[0])
	if err != nil {
		return err
	}
	nodeGwAddress := nodeLRPIP.String()
	if nodeGwAddress == "" {
		return fmt.Errorf("missing node gw router port address")
	}

	// We need to read the lsp again to get the assigned address
	lsp, err = libovsdbops.GetLogicalSwitchPort(ctx.nbcli, lsp)
	if err != nil {
		return err
	}

	if lsp.DynamicAddresses == nil || *lsp.DynamicAddresses == "" || len(strings.Split(*lsp.DynamicAddresses, " ")) < 2 {
		return fmt.Errorf("missing dynamic addresses at lsp %s", lsp.Name)
	}

	vmAddress := strings.Split(*lsp.DynamicAddresses, " ")[1]

	// Add a reroute policy to route VM n/s traffic to the node where the VM
	// is running
	policy := nbdb.LogicalRouterPolicy{
		Match:    fmt.Sprintf("ip4.src == %s", vmAddress),
		Action:   nbdb.LogicalRouterPolicyActionReroute,
		Nexthops: []string{nodeGwAddress},
		Priority: 1,
	}

	predicate := func(item *nbdb.LogicalRouterPolicy) bool {
		return item.Priority == policy.Priority && item.Match == policy.Match && item.Action == policy.Action && item.Nexthops != nil && item.Nexthop == policy.Nexthop
	}

	if err := libovsdbops.CreateOrUpdateLogicalRouterPolicyWithPredicate(ctx.nbcli, j.lr.Name, &policy, predicate); err != nil {
		return fmt.Errorf("failed ensuring policy to reroute to n/s traffic: %v", err)
	}
	return nil
}

func masqueradeTenantSubnet(ctx *CmdContext) error {
	currentGwLR := &nbdb.LogicalRouter{
		Name: ovnktypes.GWRouterPrefix + ctx.hostname,
	}

	currentGwLR, err := libovsdbops.GetLogicalRouter(ctx.nbcli, currentGwLR)
	if err != nil {
		return fmt.Errorf("failed getting current gw logical router %s: %v", currentGwLR.Name, err)
	}

	currentGwLRP := &nbdb.LogicalRouterPort{
		Name: ovnktypes.GWRouterToExtSwitchPrefix + currentGwLR.Name,
	}
	if err := ctx.nbcli.Get(context.Background(), currentGwLRP); err != nil {
		return fmt.Errorf("failed getting current gw logical router port %s: %v", currentGwLRP.Name, err)
	}

	currentGwLRPIP, _, err := net.ParseCIDR(currentGwLRP.Networks[0])
	if err != nil {
		return err
	}

	if err := ctx.nbcli.Get(context.Background(), currentGwLR); err != nil {
		return fmt.Errorf("failed getting current gw logical router %s: %v", currentGwLR.Name, err)
	}

	masqueradeNAT := &nbdb.NAT{
		ExternalIP: currentGwLRPIP.String(),
		LogicalIP:  ctx.conf.Subnet,
		Type:       nbdb.NATTypeSNAT,
		Options: map[string]string{
			"stateless": "false",
		},
	}
	if err := libovsdbops.CreateOrUpdateNATs(ctx.nbcli, currentGwLR, masqueradeNAT); err != nil {
		return fmt.Errorf("failed ensuring tenant subnet masquerade: %s")
	}
	return nil
}

func routeTenantSubnetToJoinRouter(ctx *CmdContext) error {
	joinGwPort := &nbdb.LogicalRouterPort{
		Name: ovnktypes.GWRouterToJoinSwitchPrefix + ovnktypes.OVNClusterRouter,
	}

	joinGwPort, err := libovsdbops.GetLogicalRouterPort(ctx.nbcli, joinGwPort)
	if err != nil {
		return fmt.Errorf("failed getting current join logical router port %s: %v", joinGwPort.Name, err)
	}

	joinGwPortIP, _, err := net.ParseCIDR(joinGwPort.Networks[0])
	if err != nil {
		return err
	}

	route := nbdb.LogicalRouterStaticRoute{
		IPPrefix: ctx.conf.Subnet,
		Nexthop:  joinGwPortIP.String(),
	}

	predicate := func(item *nbdb.LogicalRouterStaticRoute) bool {
		return item.Nexthop == route.Nexthop && item.IPPrefix == route.IPPrefix
	}

	nodes, err := nodes(ctx)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		if err := libovsdbops.CreateOrUpdateLogicalRouterStaticRoutesWithPredicate(ctx.nbcli, ovnktypes.GWRouterPrefix+node.Name, &route, predicate); err != nil {
			return fmt.Errorf("failed ensuring route to join router at gw: %v", err)
		}
	}

	return nil
}

func newJoinRouter() *JoinRouter {
	return &JoinRouter{
		lr: &nbdb.LogicalRouter{
			Name:    ovnktypes.OVNClusterRouter,
			Enabled: &enabled,
		},
		tenantPorts: map[string]*nbdb.LogicalRouterPort{},
		gwPorts:     map[string]*nbdb.LogicalRouterPort{},
	}
}

func (j *JoinRouter) ensure(ctx *CmdContext) error {
	if err := libovsdbops.CreateOrUpdateLogicalRouter(ctx.nbcli, j.lr); err != nil {
		return err
	}

	ports := []*nbdb.LogicalRouterPort{}
	for _, p := range j.gwPorts {
		ports = append(ports, p)
	}
	for _, p := range j.tenantPorts {
		ports = append(ports, p)
	}
	if err := libovsdbops.CreateOrUpdateLogicalRouterPorts(ctx.nbcli, j.lr, ports); err != nil {
		return err
	}
	return nil
}

func (j *JoinRouter) addTenantPort(ctx *CmdContext) {
	j.tenantPorts[ctx.conf.Name] = &nbdb.LogicalRouterPort{
		Name:     ctx.conf.Name,
		MAC:      "00:00:00:00:ff:01",
		Networks: []string{ctx.conf.Router + "/24"}, // FIXME: Use bits from conf.Subnet
		Enabled:  &enabled,
	}
}

func (j *JoinRouter) addGatewayPort(ctx *CmdContext, i int, wNode *corev1.Node) *nbdb.LogicalRouterPort {
	gwPort := &nbdb.LogicalRouterPort{
		Name:     joinToGwPortName(wNode.Name),
		MAC:      fmt.Sprintf("00:00:21:20:12:1%d", i),
		Networks: []string{fmt.Sprintf("10.64.0.1%d/24", i)},
		Enabled:  &enabled,
	}
	j.gwPorts[wNode.Name] = gwPort
	return gwPort
}

func (g *GatewayRouter) setNodePort(ctx *CmdContext, i int, wNode *corev1.Node) {
	g.gwPort = &nbdb.LogicalRouterPort{
		Name:     wNode.Name,
		MAC:      fmt.Sprintf("00:00:21:20:10:1%d", i),
		Networks: []string{fmt.Sprintf("172.19.0.25%d/16", 4-i)},
		Enabled:  &enabled,
	}
}

func (g *GatewayRouter) setJoinPort(ctx *CmdContext, i int, wNode *corev1.Node) {
	g.joinPort = &nbdb.LogicalRouterPort{
		Name:     gwToJoinPortName(wNode.Name),
		MAC:      fmt.Sprintf("00:00:21:20:11:1%d", i),
		Networks: []string{fmt.Sprintf("10.64.0.2%d/24", i)},
		Enabled:  &enabled,
	}
}

func joinToGwPortName(nodeName string) string {
	return "join-to-" + nodeName
}

func gwToJoinPortName(nodeName string) string {
	return nodeName + "-to-join"
}
