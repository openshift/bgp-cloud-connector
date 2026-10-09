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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func routing(name string, subnets ...string) *networkingapi.BGPRouting {
	return &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "cudn1"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{Name: name, Subnets: subnets},
		},
	}
}

func TestCheckAdopted(t *testing.T) {
	asn := func(n int64) *networkingapi.BGPCloudConfiguration {
		c := &networkingapi.BGPCloudConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
		c.Spec.BGP.LocalASN = n
		return c
	}
	ns := func(labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod", Labels: labels}}
	}

	for _, tc := range []struct {
		name    string
		desired any
		stored  any
		want    string // substring of the error, or "" for no error
	}{
		{
			name:    "identical spec is adopted",
			desired: routing("prod", "10.100.0.0/16"),
			stored:  routing("prod", "10.100.0.0/16"),
		},
		{
			name:    "a field the manifest sets differs",
			desired: routing("prod", "10.100.0.0/16"),
			stored:  routing("staging", "10.100.0.0/16"),
			want:    "spec.network.name",
		},
		{
			name:    "a list the manifest sets differs",
			desired: routing("prod", "10.100.0.0/16"),
			stored:  routing("prod", "10.200.0.0/16"),
			want:    "spec.network.subnets",
		},
		{
			name:    "a scalar the manifest sets differs",
			desired: asn(65001),
			stored:  asn(65002),
			want:    "spec.bgp.localASN",
		},
		{
			name:    "a label the manifest sets is missing",
			desired: ns(map[string]string{"cluster-udn": "prod"}),
			stored:  ns(map[string]string{"other": "x"}),
			want:    "metadata.labels.cluster-udn",
		},
		{
			name:    "extra labels on the stored object are ignored",
			desired: ns(map[string]string{"cluster-udn": "prod"}),
			stored:  ns(map[string]string{"cluster-udn": "prod", "kubernetes.io/metadata.name": "prod"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAdopted(tc.desired.(adoptable), tc.stored.(adoptable))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("expected the object to be adopted, got %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("expected a mismatch on %s, got none", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("expected the mismatch to name %s, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckAdoptedIgnoresFieldsOnlyTheClusterSet(t *testing.T) {
	desired := &networkingapi.BGPCloudConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	desired.Spec.BGP.LocalASN = 65001

	stored := desired.DeepCopy()
	// A value the API server or a defaulting webhook filled in; the
	// manifest never named it, so it cannot disagree with it.
	stored.Spec.BGP.LivenessDetection = networkingapi.LivenessDetectionType("bgp-keepalive")

	if err := CheckAdopted(desired, stored); err != nil {
		t.Fatalf("a field only the cluster set should not count as a mismatch: %v", err)
	}
}

func TestCreateOrReuse(t *testing.T) {
	namespace := func(cudn string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   "prod",
			Labels: map[string]string{"cluster-udn": cudn},
		}}
	}

	for _, tc := range []struct {
		name     string
		existing *corev1.Namespace
		reuse    bool
		check    func(t *testing.T, err error)
	}{
		{"absent", nil, false, func(t *testing.T, err error) {
			if err != nil {
				t.Errorf("creating an absent object: %v", err)
			}
		}},
		{"absent, reusing", nil, true, func(t *testing.T, err error) {
			if err != nil {
				t.Errorf("creating an absent object: %v", err)
			}
		}},
		{"present, not reusing", namespace("prod"), false, func(t *testing.T, err error) {
			if !apierrors.IsAlreadyExists(err) {
				t.Errorf("want AlreadyExists when not reusing, got %v", err)
			}
		}},
		{"present and matching, reusing", namespace("prod"), true, func(t *testing.T, err error) {
			if err != nil {
				t.Errorf("adopting a matching object: %v", err)
			}
		}},
		{"present and different, reusing", namespace("other"), true, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "E2E_REUSE_CRS") ||
				!strings.Contains(err.Error(), "cluster-udn") {
				t.Errorf("want an E2E_REUSE_CRS error naming the label that differs, got %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme)
			if tc.existing != nil {
				b = b.WithObjects(tc.existing)
			}
			c := b.Build()

			obj := namespace("prod")
			err := CreateOrReuse(context.Background(), c, obj, tc.reuse)
			tc.check(t, err)
			if err == nil && obj.ResourceVersion == "" {
				t.Errorf("obj was not read back from the cluster")
			}
		})
	}
}
