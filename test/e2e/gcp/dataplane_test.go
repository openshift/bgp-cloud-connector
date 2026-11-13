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

package gcp_e2e

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"google.golang.org/api/compute/v1"

	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

// The GCP half of the data-plane check: how the client VM is found and
// driven, and how a node's instance says whether it forwards. The rule
// itself, and the netexec pods it is checked against, are in test/e2e.

const (
	// How long one round may take to appear on the client's console.
	roundTimeout = 3 * time.Minute

	// The metadata attribute the client reads its round and addresses
	// from, and the prefix of what it prints.
	// hack/gcp/create-client-vm.sh uses the same name.
	probeAttribute = "e2e-probe"

	// The label hack/gcp/create-client-vm.sh puts on its VM, with the
	// infra id as its value.
	clientLabel = "bgp-cloud-connector-e2e-client"
)

// clientVM is the VM hack/gcp/create-client-vm.sh built for this
// cluster.
type clientVM struct {
	name, zone, address string
}

// findClientVM finds the client by its label. The label is compared
// here as well as in the filter, because a Compute filter is a match,
// not an identity test.
func findClientVM(ctx context.Context) (clientVM, error) {
	project := bgpConfig.Spec.GCP.Project
	var found []clientVM
	err := computeSvc.Instances.AggregatedList(project).
		Filter(fmt.Sprintf("labels.%s=%s", clientLabel, clusterID)).
		Pages(ctx, func(page *compute.InstanceAggregatedList) error {
			for _, scoped := range page.Items {
				for _, inst := range scoped.Instances {
					if inst.Labels[clientLabel] != clusterID || inst.Status != "RUNNING" ||
						len(inst.NetworkInterfaces) == 0 {
						continue
					}
					found = append(found, clientVM{
						name:    inst.Name,
						zone:    path.Base(inst.Zone),
						address: inst.NetworkInterfaces[0].NetworkIP,
					})
				}
			}
			return nil
		})
	if err != nil {
		return clientVM{}, err
	}
	if len(found) != 1 {
		return clientVM{}, fmt.Errorf("want one running VM labelled %s=%s, found %d; "+
			"create it with hack/gcp/create-client-vm.sh", clientLabel, clusterID, len(found))
	}
	return found[0], nil
}

// destinations pairs every worker's netexec pod with whether the
// worker's instance forwards: canIpForward, as the operator sets it on
// router nodes.
func destinations(ctx context.Context, namespace string) ([]e2e.Destination, error) {
	workers, addresses, err := e2e.NetexecPods(ctx, k8sClient, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]e2e.Destination, 0, len(workers))
	for i := range workers {
		node := &workers[i]
		inst, err := instanceFor(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("reading the instance behind %s: %w", node.Name, err)
		}
		out = append(out, e2e.Destination{
			Node:       node.Name,
			PodAddress: addresses[node.Name],
			Forwarding: inst.CanIpForward,
		})
	}
	return out, nil
}

// probe has the client VM request /clientip from every address and
// returns what each address answered. An address that did not answer
// maps to "".
//
// Nothing connects to the VM. hack/gcp/create-client-vm.sh gives it a
// loop that reads the VM's own probeAttribute from the metadata server,
// probes the addresses named there whenever the round in it changes, and
// prints the results to the serial console. So driving it needs only
// Compute calls -- setMetadata and getSerialPortOutput -- and no service
// account, agent or inbound rule on the VM.
func probe(ctx context.Context, vm clientVM, addresses []string) (map[string]string, error) {
	project := bgpConfig.Spec.GCP.Project
	round := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := setProbeAttribute(ctx, project, vm, round+" "+strings.Join(addresses, " ")); err != nil {
		return nil, fmt.Errorf("asking the client for round %s: %w", round, err)
	}

	deadline := time.Now().Add(roundTimeout)
	for {
		out, err := computeSvc.Instances.GetSerialPortOutput(project, vm.zone, vm.name).
			Port(1).Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("reading the client's console: %w", err)
		}
		if results, complete := e2e.ProbeRound(out.Contents, round); complete {
			GinkgoWriter.Printf("client VM said:\n%s\n", results)
			return e2e.ParseProbeOutput(results, addresses)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("round %s did not appear on the client's console within %s", round, roundTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// setProbeAttribute replaces probeAttribute in the VM's metadata and
// waits for the change to be applied. setMetadata takes the whole set
// with the fingerprint it was read at, so the other items -- the startup
// script among them -- are carried over unchanged.
func setProbeAttribute(ctx context.Context, project string, vm clientVM, value string) error {
	inst, err := computeSvc.Instances.Get(project, vm.zone, vm.name).Context(ctx).Do()
	if err != nil {
		return err
	}
	md := inst.Metadata
	if md == nil {
		md = &compute.Metadata{}
	}
	items := make([]*compute.MetadataItems, 0, len(md.Items)+1)
	for _, item := range md.Items {
		if item.Key != probeAttribute {
			items = append(items, item)
		}
	}
	items = append(items, &compute.MetadataItems{Key: probeAttribute, Value: &value})
	op, err := computeSvc.Instances.SetMetadata(project, vm.zone, vm.name, &compute.Metadata{
		Fingerprint: md.Fingerprint,
		Items:       items,
	}).Context(ctx).Do()
	if err != nil {
		return err
	}
	return waitZoneOp(ctx, vm.zone, op)
}
