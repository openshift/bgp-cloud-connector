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

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

type vmHostRoutesByNode map[string][]string

var errVMIAPIUnavailable = errors.New("VirtualMachineInstance API is unavailable")

// VMHostRouteStatus is what one pass of EnsureVMHostRoutes achieved. Pending
// and Unserved are independent: routes can be written for some VMs while
// others wait for an address and others cannot be given a route at all.
type VMHostRouteStatus struct {
	// Configured counts the prefixes written across every node.
	Configured int
	// Pending is set when a VMI has no usable address yet, or names a node
	// that no longer exists. Both clear without anyone intervening.
	Pending bool
	// Unserved describes VMs that cannot be given a host route as the cluster
	// stands, one entry per reason. Clearing these needs a change to the
	// cluster, so they are reported rather than retried.
	Unserved []string
}

func EnsureVMHostRoutes(ctx context.Context, c client.Client, routing *networkingapi.BGPRouting, config *networkingapi.BGPCloudConfiguration) (VMHostRouteStatus, error) {
	routes, nodes, pending, err := discoverVMHostRoutes(ctx, c, routing)
	if err != nil {
		if errors.Is(err, errVMIAPIUnavailable) {
			hasExisting, listErr := hasVMHostRouteConfigurations(ctx, c, routing.Name)
			if listErr != nil {
				return VMHostRouteStatus{}, listErr
			}
			// An unavailable optional VMI API means "unknown," not "no VMIs."
			// Preserve existing routes; without any, there is nothing to withdraw.
			if !hasExisting {
				return VMHostRouteStatus{}, nil
			}
		}
		return VMHostRouteStatus{}, err
	}

	groups := effectivePeerGroups(config)
	expected := make(map[string]struct{}, len(routes))
	var skipped []string
	var unsupported []string
	total := 0
	for nodeName, prefixes := range routes {
		node := nodes[nodeName]
		neighbors := neighborsForNode(config, groups, node)
		if len(neighbors) == 0 {
			skipped = append(skipped, nodeName)
			continue
		}

		supportedPrefixes, missingPrefixes := prefixesSupportedByNeighbors(prefixes, neighbors)
		if len(missingPrefixes) > 0 {
			unsupported = append(unsupported, fmt.Sprintf("%s: %s", nodeName, strings.Join(missingPrefixes, ", ")))
		}
		if len(supportedPrefixes) == 0 {
			continue
		}

		name := vmHostRouteConfigurationName(routing.Name, nodeName)
		expected[name] = struct{}{}
		if err := ensureVMHostRouteConfiguration(ctx, c, routing, config, node, neighbors, supportedPrefixes, name); err != nil {
			return VMHostRouteStatus{}, err
		}
		total += len(supportedPrefixes)
	}

	if err := pruneVMHostRouteConfigurations(ctx, c, routing.Name, expected); err != nil {
		return VMHostRouteStatus{}, err
	}

	status := VMHostRouteStatus{Configured: total, Pending: pending}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		status.Unserved = append(status.Unserved,
			fmt.Sprintf("VMs are running on nodes without matching BGP peers: %s", strings.Join(skipped, ", ")))
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		status.Unserved = append(status.Unserved,
			fmt.Sprintf("VM host routes have no BGP neighbour of the same address family: %s", strings.Join(unsupported, "; ")))
	}
	return status, nil
}

func discoverVMHostRoutes(ctx context.Context, c client.Client, routing *networkingapi.BGPRouting) (vmHostRoutesByNode, map[string]corev1.Node, bool, error) {
	namespaces := &corev1.NamespaceList{}
	if err := c.List(ctx, namespaces, client.MatchingLabels{
		LabelClusterUDN: routing.Spec.Network.Name,
		LabelPrimaryUDN: "",
	}); err != nil {
		return nil, nil, false, err
	}
	selectedNamespaces := make(map[string]struct{}, len(namespaces.Items))
	for i := range namespaces.Items {
		selectedNamespaces[namespaces.Items[i].Name] = struct{}{}
	}

	vmis := make([]unstructured.Unstructured, 0)
	for namespace := range selectedNamespaces {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(VirtualMachineInstanceGVK.GroupVersion().WithKind("VirtualMachineInstanceList"))
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
				return nil, nil, false, fmt.Errorf("%w: %w", errVMIAPIUnavailable, err)
			}
			return nil, nil, false, err
		}
		vmis = append(vmis, list.Items...)
	}

	prefixes, err := parseNetworkPrefixes(routing.Spec.Network.Subnets)
	if err != nil {
		return nil, nil, false, err
	}
	nodes := make(map[string]corev1.Node)

	routes := vmHostRoutesByNode{}
	pending := false
	for i := range vmis {
		vmi := &vmis[i]
		if !vmi.GetDeletionTimestamp().IsZero() {
			continue
		}
		phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
		nodeName, _, _ := unstructured.NestedString(vmi.Object, "status", "nodeName")
		if phase != "Running" {
			if phase == "Succeeded" || phase == "Failed" {
				continue
			}
			pending = true
			continue
		}
		if nodeName == "" {
			pending = true
			continue
		}
		if _, found := nodes[nodeName]; !found {
			node := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
				if apierrors.IsNotFound(err) {
					// The node is gone while the VMI still names it. There is
					// nothing to advertise from until KubeVirt moves the VMI to
					// a terminal phase or reschedules it, so skip this one and
					// leave the remaining VMIs and the prune to run.
					pending = true
					continue
				}
				return nil, nil, false, fmt.Errorf("getting node %q hosting VMI %s/%s: %w", nodeName, vmi.GetNamespace(), vmi.GetName(), err)
			}
			nodes[nodeName] = *node
		}

		matchedNetwork := false
		for _, address := range vmiAddresses(vmi) {
			ip, err := netip.ParseAddr(address)
			if err != nil {
				continue
			}
			for _, network := range prefixes {
				if network.Contains(ip) {
					routes[nodeName] = append(routes[nodeName], netip.PrefixFrom(ip, ip.BitLen()).String())
					matchedNetwork = true
					break
				}
			}
		}
		// A VMI need not have an address in every subnet of a dual-stack
		// network. It is ready for this controller once any address belongs to
		// the routed network; advertise every such address that is present.
		if !matchedNetwork {
			pending = true
		}
	}
	for nodeName := range routes {
		routes[nodeName] = sortedUnique(routes[nodeName])
	}
	return routes, nodes, pending, nil
}

func parseNetworkPrefixes(subnets []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(subnets))
	for _, subnet := range subnets {
		prefix, err := netip.ParsePrefix(subnet)
		if err != nil {
			return nil, fmt.Errorf("parsing network subnet %q: %w", subnet, err)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

func vmiAddresses(vmi *unstructured.Unstructured) []string {
	interfaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	var addresses []string
	for _, item := range interfaces {
		iface, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if values, found, _ := unstructured.NestedStringSlice(iface, "ipAddresses"); found && len(values) > 0 {
			addresses = append(addresses, values...)
			continue
		}
		if value, found, _ := unstructured.NestedString(iface, "ipAddress"); found {
			addresses = append(addresses, value)
		}
	}
	return addresses
}

func effectivePeerGroups(config *networkingapi.BGPCloudConfiguration) []platform.PeerGroup {
	if config.Spec.Platform == networkingapi.PlatformManual {
		return peerGroupsFromSpec(config)
	}
	groups := make([]platform.PeerGroup, 0, len(config.Status.PeerGroups))
	for _, status := range config.Status.PeerGroups {
		group := platform.PeerGroup{Key: status.Key, NodeSelector: status.NodeSelector}
		for _, neighbor := range status.Neighbors {
			group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
				Address: neighbor.Address, ASN: neighbor.RemoteASN, EBGPMultiHop: neighbor.EBGPMultiHop,
			})
		}
		groups = append(groups, group)
	}
	return groups
}

func neighborsForNode(config *networkingapi.BGPCloudConfiguration, groups []platform.PeerGroup, node corev1.Node) []platform.DiscoveredNeighbor {
	if !labels.SelectorFromSet(config.Spec.RouterNodeSelector).Matches(labels.Set(node.Labels)) {
		return nil
	}
	byKey := map[string]platform.DiscoveredNeighbor{}
	for _, group := range groups {
		if !labels.SelectorFromSet(group.NodeSelector).Matches(labels.Set(node.Labels)) {
			continue
		}
		for _, neighbor := range group.Neighbors {
			byKey[fmt.Sprintf("%s/%d", neighbor.Address, neighbor.ASN)] = neighbor
		}
	}
	neighbors := make([]platform.DiscoveredNeighbor, 0, len(byKey))
	for _, neighbor := range byKey {
		neighbors = append(neighbors, neighbor)
	}
	sort.Slice(neighbors, func(i, j int) bool {
		if neighbors[i].Address == neighbors[j].Address {
			return neighbors[i].ASN < neighbors[j].ASN
		}
		return neighbors[i].Address < neighbors[j].Address
	})
	return neighbors
}

func prefixesSupportedByNeighbors(prefixes []string, neighbors []platform.DiscoveredNeighbor) ([]string, []string) {
	supported := make([]string, 0, len(prefixes))
	missing := make([]string, 0)
	for _, prefix := range prefixes {
		parsedPrefix, err := netip.ParsePrefix(prefix)
		if err != nil {
			missing = append(missing, prefix)
			continue
		}
		found := false
		for _, neighbor := range neighbors {
			address, err := netip.ParseAddr(neighbor.Address)
			if err == nil && address.Is4() == parsedPrefix.Addr().Is4() {
				found = true
				break
			}
		}
		if found {
			supported = append(supported, prefix)
		} else {
			missing = append(missing, prefix)
		}
	}
	return supported, missing
}

func prefixesForNeighbor(prefixes []string, neighborAddress string) []interface{} {
	address, err := netip.ParseAddr(neighborAddress)
	if err != nil {
		return nil
	}
	values := make([]interface{}, 0, len(prefixes))
	for _, prefix := range prefixes {
		parsedPrefix, err := netip.ParsePrefix(prefix)
		if err == nil && address.Is4() == parsedPrefix.Addr().Is4() {
			values = append(values, prefix)
		}
	}
	return values
}

func ensureVMHostRouteConfiguration(ctx context.Context, c client.Client, routing *networkingapi.BGPRouting, config *networkingapi.BGPCloudConfiguration, node corev1.Node, neighbors []platform.DiscoveredNeighbor, prefixes []string, name string) error {
	prefixValues := make([]interface{}, len(prefixes))
	for i := range prefixes {
		prefixValues[i] = prefixes[i]
	}
	neighborValues := make([]interface{}, 0, len(neighbors))
	for _, discovered := range neighbors {
		allowedPrefixes := prefixesForNeighbor(prefixes, discovered.Address)
		if len(allowedPrefixes) == 0 {
			continue
		}
		neighbor := frrNeighborBase(config, discovered)
		neighbor["toAdvertise"] = map[string]interface{}{
			"allowed": map[string]interface{}{
				"mode":     "filtered",
				"prefixes": allowedPrefixes,
			},
		}
		neighborValues = append(neighborValues, neighbor)
	}

	hostname := node.Labels[corev1.LabelHostname]
	if hostname == "" {
		hostname = node.Name
	}
	bgp := map[string]interface{}{
		"routers": []interface{}{map[string]interface{}{
			"asn":       config.Spec.BGP.LocalASN,
			"prefixes":  prefixValues,
			"neighbors": neighborValues,
		}},
	}
	if config.Spec.BGP.LivenessDetection == networkingapi.LivenessDetectionBFD {
		bgp["bfdProfiles"] = []interface{}{frrBFDProfile()}
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1",
		"kind":       "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name":        name,
			"namespace":   FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: routing.Name},
		},
		"spec": map[string]interface{}{
			"nodeSelector": map[string]interface{}{"matchLabels": map[string]interface{}{corev1.LabelHostname: hostname}},
			"bgp":          bgp,
		},
	}}
	// The owner reference lets garbage collection remove the namespaced route
	// even if BGPRouting deletion bypasses its finalizer. This prevents an
	// orphaned FRRConfiguration from continuing to originate stale prefixes.
	if err := controllerutil.SetControllerReference(routing, obj, c.Scheme()); err != nil {
		return fmt.Errorf("setting owner reference on %s: %w", name, err)
	}
	return createOrUpdate(ctx, c, obj, nil)
}

func pruneVMHostRouteConfigurations(ctx context.Context, c client.Client, routingName string, expected map[string]struct{}) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"))
	if err := c.List(ctx, list, client.InNamespace(FRRNamespace), client.MatchingLabels{LabelManagedBy: LabelManagedByVMHostRoutes}); err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetAnnotations()[AnnotationBGPRouting] != routingName {
			continue
		}
		if _, found := expected[item.GetName()]; found {
			continue
		}
		if err := c.Delete(ctx, item); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func hasVMHostRouteConfigurations(ctx context.Context, c client.Client, routingName string) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"))
	if err := c.List(ctx, list, client.InNamespace(FRRNamespace), client.MatchingLabels{LabelManagedBy: LabelManagedByVMHostRoutes}); err != nil {
		return false, err
	}
	for i := range list.Items {
		if list.Items[i].GetAnnotations()[AnnotationBGPRouting] == routingName {
			return true, nil
		}
	}
	return false, nil
}

func DeleteVMHostRoutes(ctx context.Context, c client.Client, routingName string) error {
	return pruneVMHostRouteConfigurations(ctx, c, routingName, map[string]struct{}{})
}

func vmHostRouteConfigurationName(routingName, nodeName string) string {
	sum := sha256.Sum256([]byte(routingName + "\x00" + nodeName))
	return fmt.Sprintf("%s%x", VMHostRouteNamePrefix, sum[:8])
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}
