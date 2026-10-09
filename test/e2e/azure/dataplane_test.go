/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure_e2e

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

// The Azure half of the data-plane check: how the client VM is found and
// driven, and how a node's interface says whether it forwards. The rule
// itself, and the netexec pods it is checked against, are in test/e2e.

const (
	// How long the client VM waits for one pod. A pod that is reachable
	// answers in milliseconds; this only bounds the wait on one that is
	// not.
	probeTimeoutSeconds = 10

	// How long one az invocation may take. A run-command answered in
	// thirty to forty seconds when measured, and Azure allows one to run
	// for up to ninety minutes; Eventually stops retrying at its own
	// timeout but cannot interrupt a call already in progress, so each
	// call carries its own deadline.
	azTimeout = 3 * time.Minute
)

// clientVM names the VM hack/azure/create-client-vm.sh builds. Derived
// rather than passed, so the script and the suite agree by construction
// on a cluster neither was told about.
func clientVM() (name, resourceGroup string) {
	return clusterID + "-e2e-client", bgpConfig.Spec.Azure.ResourceGroup
}

// az runs the Azure CLI against the profile's subscription and returns
// its stdout. The suite drives the client VM through `az vm run-command`
// rather than through a new SDK module: the CLI is already what the CI
// step logs in with, and the call goes through the VM agent rather than
// the network, so the VM needs no public address and no inbound rule.
func az(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, azTimeout)
	defer cancel()
	args = append(args, "--subscription", bgpConfig.Spec.Azure.SubscriptionID)
	cmd := exec.CommandContext(ctx, "az", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("az %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// clientVMAddress reads the client VM's private address from Azure. The
// pods are expected to report exactly this as the source of every
// request.
func clientVMAddress(ctx context.Context) (string, error) {
	name, rg := clientVM()
	addr, err := az(ctx, "vm", "list-ip-addresses", "-g", rg, "-n", name,
		"--query", "[0].virtualMachine.network.privateIpAddresses[0]", "-o", "tsv")
	if err != nil {
		return "", err
	}
	if net.ParseIP(addr) == nil {
		return "", fmt.Errorf("no client VM %s in %s (got %q); create it with hack/azure/create-client-vm.sh", name, rg, addr)
	}
	return addr, nil
}

// destinations pairs every worker's netexec pod with whether the worker's
// interface forwards, read from Azure: enableIPForwarding on its NIC.
func destinations(ctx context.Context, namespace string) ([]e2e.Destination, error) {
	workers, addresses, err := e2e.NetexecPods(ctx, k8sClient, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]e2e.Destination, 0, len(workers))
	for i := range workers {
		node := &workers[i]
		nic, err := nicForNode(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("reading the interface of %s: %w", node.Name, err)
		}
		if nic == nil || nic.Properties == nil {
			return nil, fmt.Errorf("no interface found for %s", node.Name)
		}
		out = append(out, e2e.Destination{
			Node:       node.Name,
			PodAddress: addresses[node.Name],
			Forwarding: nic.Properties.EnableIPForwarding != nil && *nic.Properties.EnableIPForwarding,
		})
	}
	return out, nil
}

// probe sends one request to every address from the client VM, in a
// single run-command, and returns what each address answered. An
// address that did not answer maps to "".
//
// One invocation rather than one per address because each costs about
// thirty seconds of agent round trip, which would dominate the run.
func probe(ctx context.Context, addresses []string) (map[string]string, error) {
	name, rg := clientVM()
	msg, err := az(ctx, "vm", "run-command", "invoke", "-g", rg, "-n", name,
		"--command-id", "RunShellScript", "--scripts", e2e.ProbeScript(addresses, probeTimeoutSeconds),
		"--query", "value[0].message", "-o", "tsv")
	if err != nil {
		return nil, err
	}
	GinkgoWriter.Printf("client VM said:\n%s\n", msg)
	return e2e.ParseProbeOutput(runCommandStdout(msg), addresses)
}

// runCommandStdout takes the script's stdout out of the agent's report:
// a status line, then stdout between [stdout] and [stderr].
func runCommandStdout(msg string) string {
	var out []string
	inStdout := false
	for _, line := range strings.Split(msg, "\n") {
		switch strings.TrimSpace(line) {
		case "[stdout]":
			inStdout = true
			continue
		case "[stderr]":
			inStdout = false
			continue
		}
		if inStdout {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
