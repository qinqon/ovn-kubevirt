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

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/ovn-org/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

// PluginConf is whatever you expect your configuration json to be. This is whatever
// is passed in on stdin. Your plugin may wish to expose its functionality via
// runtime args, see CONVENTIONS.md in the CNI spec.
type PluginConf struct {
	// This embeds the standard NetConf structure which allows your plugin
	// to more easily parse standard fields like Name, Type, CNIVersion,
	// and PrevResult.
	types.NetConf

	RuntimeConfig *struct {
		SampleConfig map[string]interface{} `json:"sample"`
	} `json:"runtimeConfig"`

	// Add plugin-specifc flags here
	MyAwesomeFlag     bool   `json:"myAwesomeFlag"`
	AnotherAwesomeArg string `json:"anotherAwesomeArg"`
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
	// End previous result parsing

	// Do any validation here
	if conf.AnotherAwesomeArg == "" {
		return nil, fmt.Errorf("anotherAwesomeArg must be specified")
	}

	return &conf, nil
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
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
	_, err = current.GetResult(cniConf.PrevResult)
	if err != nil {
		return fmt.Errorf("failed to convert prevResult: %v", err)
	}

	cli, err := newClient()
	if err != nil {
		return err
	}

	err = cli.Connect(context.Background())
	if err != nil {
		return err
	}

	if err := libovsdbops.CreateOrUpdateLogicalSwitchPortsAndSwitch(cli, &nbdb.LogicalSwitch{Name: cniConf.Name}, &nbdb.LogicalSwitchPort{Name: cniConf.Name}); err != nil {
		return err
	}

	return types.PrintResult(&current.Result{}, cniConf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	cniConf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	cli, err := newClient()
	if err != nil {
		return err
	}

	if err := libovsdbops.DeleteLogicalSwitchPorts(cli, &nbdb.LogicalSwitch{Name: cniConf.Name}, &nbdb.LogicalSwitchPort{Name: cniConf.Name}); err != nil {
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
