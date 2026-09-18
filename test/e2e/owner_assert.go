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

package e2e

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// CheckOwnedByConfig reports whether obj names the configuration among
// its owners, which is how an object the operator creates on the
// configuration's behalf is collected when the configuration goes. A
// missing reference costs nothing until the day something has to tidy
// up, which is why it is worth asserting while everything still works.
func CheckOwnedByConfig(obj *unstructured.Unstructured, config *networkingapi.BGPCloudConfiguration) error {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "BGPCloudConfiguration" && ref.UID == config.UID {
			return nil
		}
	}
	return fmt.Errorf("%s %s/%s does not name BGPCloudConfiguration %s (uid %s) as an owner; it has %v",
		obj.GetKind(), obj.GetNamespace(), obj.GetName(), config.Name, config.UID, obj.GetOwnerReferences())
}
