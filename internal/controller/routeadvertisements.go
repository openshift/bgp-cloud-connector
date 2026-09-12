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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func EnsureRouteAdvertisements(ctx context.Context, c client.Client) error {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.ovn.org/v1",
			"kind":       "RouteAdvertisements",
			"metadata": map[string]interface{}{
				"name": RouteAdvertisementName,
				"labels": map[string]interface{}{
					LabelManagedBy: LabelManagedByVal,
				},
			},
			"spec": map[string]interface{}{
				"nodeSelector":             map[string]interface{}{},
				"frrConfigurationSelector": map[string]interface{}{},
				"networkSelectors": []interface{}{
					map[string]interface{}{
						"networkSelectionType": "ClusterUserDefinedNetworks",
						"clusterUserDefinedNetworkSelector": map[string]interface{}{
							"networkSelector": map[string]interface{}{
								"matchLabels": map[string]interface{}{
									LabelAdvertise: "true",
								},
							},
						},
					},
				},
				"advertisements": []interface{}{"PodNetwork"},
			},
		},
	}

	return createOrUpdate(ctx, c, obj, nil)
}

func DeleteRouteAdvertisements(ctx context.Context, c client.Client) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(RouteAdvertisementsGVK)
	obj.SetName(RouteAdvertisementName)

	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
