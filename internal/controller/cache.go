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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CacheOptions limits the Pod informer to KubeVirt launcher Pods at the API
// server, including its initial LIST.
func CacheOptions() cache.Options {
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}: {Label: labels.SelectorFromSet(labels.Set{"kubevirt.io": "virt-launcher"})},
	}}
}

// ClientOptions keeps secrets and Pods out of the manager's cache.
//
// The operator reads exactly one secret -- the credentials the cloud
// credential operator writes for it -- and is granted get on exactly
// that name. A cached read cannot be that narrow: controller-runtime
// serves typed reads from an informer, an informer needs list and watch
// over a whole namespace, and resourceNames does not apply to collection
// verbs. So a cached secret forces the Role to permit every secret in
// the namespace, which is far more than the operator needs and more than
// it should be able to hold in memory.
//
// Read straight from the API server instead. It costs one live GET on a
// reconcile that already makes a live sts:GetCallerIdentity call, and it
// buys a Role that names the single secret the operator is entitled to.
//
// Pods are also read directly. The operator only lists the small set of
// frr-k8s Pods during cloud-configuration reconciliation; caching that list
// would otherwise retain every Pod in the cluster. VM lifecycle events use
// either the VirtualMachineInstance informer or the separately filtered
// virt-launcher Pod informer.
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}, &corev1.Pod{}},
		},
	}
}
