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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// CUDNValidationError is returned when the Kubernetes API server rejects a CUDN
// object as structurally invalid (e.g. bad CIDR in spec.network.layer2.subnets).
// This is a terminal condition — the user must correct spec.network in the BGPRouting.
type CUDNValidationError struct {
	Cause error
}

func (e *CUDNValidationError) Error() string {
	return fmt.Sprintf("CUDN spec is invalid and must be corrected: %v", e.Cause)
}

func (e *CUDNValidationError) Unwrap() error { return e.Cause }

// MatchedNamespaces returns the sorted names of the namespaces selected into the
// given network by their labels. Users must create and label namespaces
// themselves, so an empty match is an error the caller reports as degraded; the
// returned names are reported in status so an administrator can see exactly
// which namespaces the network advertises without re-deriving the label query.
func MatchedNamespaces(ctx context.Context, c client.Client, networkName string) ([]string, error) {
	nsList := &corev1.NamespaceList{}
	if err := c.List(ctx, nsList, client.MatchingLabels{
		LabelPrimaryUDN: "",
		LabelClusterUDN: networkName,
	}); err != nil {
		return nil, err
	}
	if len(nsList.Items) == 0 {
		return nil, fmt.Errorf("no namespace found with labels %s=\"\" and %s=%q; create and label a namespace before applying BGPRouting",
			LabelPrimaryUDN, LabelClusterUDN, networkName)
	}
	names := make([]string, 0, len(nsList.Items))
	for i := range nsList.Items {
		names = append(names, nsList.Items[i].Name)
	}
	sort.Strings(names)
	return names, nil
}

func EnsureClusterUDN(ctx context.Context, c client.Client, routing *networkingapi.BGPRouting) error {
	name := ClusterUDNNamePrefix + routing.Spec.Network.Name

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.ovn.org/v1",
			"kind":       "ClusterUserDefinedNetwork",
			"metadata": map[string]interface{}{
				"name": name,
				"labels": map[string]interface{}{
					LabelAdvertise: "true",
					LabelManagedBy: LabelManagedByVal,
				},
			},
			"spec": map[string]interface{}{
				"namespaceSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						LabelClusterUDN: routing.Spec.Network.Name,
					},
				},
				"network": map[string]interface{}{
					"topology": "Layer2",
					"layer2": map[string]interface{}{
						"role": "Primary",
						"ipam": map[string]interface{}{
							"lifecycle": "Persistent",
						},
						"subnets": toSubnetInterfaces(routing.Spec.Network.Subnets),
					},
				},
			},
		},
	}

	return createOrUpdateCUDN(ctx, c, obj)
}

// clusterUDNNetworkCreated reports the observed NetworkCreated condition. The
// condition is optional immediately after a CUDN is created; in that case the
// caller keeps the existing optimistic status until OVN publishes its first
// observation. Once present, any status other than True means the CUDN is not
// serving the requested network.
func clusterUDNNetworkCreated(ctx context.Context, c client.Client, networkName string) (observed, ready bool, message string, err error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ClusterUDNGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: ClusterUDNNamePrefix + networkName}, obj); err != nil {
		return false, false, "", err
	}

	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil {
		return false, false, "", fmt.Errorf("reading ClusterUDN %q conditions: %w", obj.GetName(), err)
	}
	if !found {
		return false, false, "", nil
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]interface{})
		if !ok {
			return false, false, "", fmt.Errorf("reading ClusterUDN %q conditions: expected an object", obj.GetName())
		}
		typeName, _, err := unstructured.NestedString(condition, "type")
		if err != nil {
			return false, false, "", fmt.Errorf("reading ClusterUDN %q condition type: %w", obj.GetName(), err)
		}
		if typeName != "NetworkCreated" {
			continue
		}
		status, _, err := unstructured.NestedString(condition, "status")
		if err != nil {
			return false, false, "", fmt.Errorf("reading ClusterUDN %q NetworkCreated status: %w", obj.GetName(), err)
		}
		reason, _, err := unstructured.NestedString(condition, "reason")
		if err != nil {
			return false, false, "", fmt.Errorf("reading ClusterUDN %q NetworkCreated reason: %w", obj.GetName(), err)
		}
		conditionMessage, _, err := unstructured.NestedString(condition, "message")
		if err != nil {
			return false, false, "", fmt.Errorf("reading ClusterUDN %q NetworkCreated message: %w", obj.GetName(), err)
		}
		if status == "True" {
			return true, true, "", nil
		}
		return true, false, fmt.Sprintf("condition is %s (%s): %s", status, reason, conditionMessage), nil
	}
	return false, false, "", nil
}

func createOrUpdateCUDN(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	err := createOrUpdate(ctx, c, obj, nil)
	if err != nil && apierrors.IsInvalid(err) {
		return &CUDNValidationError{Cause: err}
	}
	return err
}

func DeleteClusterUDN(ctx context.Context, c client.Client, networkName string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ClusterUDNGVK)
	obj.SetName(ClusterUDNNamePrefix + networkName)

	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func toSubnetInterfaces(subnets []string) []interface{} {
	out := make([]interface{}, len(subnets))
	for i, s := range subnets {
		out[i] = s
	}
	return out
}
