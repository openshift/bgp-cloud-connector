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

package gcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const testNamespace = "openshift-bgp-cloud-connector"

func credentialsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("registering the client-go scheme: %v", err)
	}
	s.AddKnownTypeWithName(CredentialsRequestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		CredentialsRequestGVK.GroupVersion().WithKind("CredentialsRequestList"),
		&unstructured.UnstructuredList{})
	return s
}

// serviceAccountJSON is the shape the cloud credential operator writes in
// mint mode. Nothing here is a real key.
const serviceAccountJSON = `{
  "type": "service_account",
  "project_id": "openshift-qe",
  "client_email": "test@openshift-qe.iam.gserviceaccount.com",
  "private_key_id": "0000",
  "private_key": "-----BEGIN PRIVATE KEY-----\nnot-a-key\n-----END PRIVATE KEY-----\n"
}`

// noAmbient stands in for a pod, where the default chain finds nothing
// because the metadata server is unreachable.
func noAmbient(t *testing.T) {
	t.Helper()
	previous := ambientCredentials
	ambientCredentials = func(context.Context) ([]byte, error) {
		return nil, errors.New("no ambient credentials")
	}
	t.Cleanup(func() { ambientCredentials = previous })
}

// TestResolveCredentials_CreatesRequestAndWaits pins the path a pod takes
// on a cluster where nothing has been minted yet: ask, and say plainly
// that the answer has not arrived, rather than failing.
func TestResolveCredentials_CreatesRequestAndWaits(t *testing.T) {
	noAmbient(t)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace)
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err != nil {
		t.Fatalf("the request was not raised: %v", err)
	}
	kind, _, _ := unstructured.NestedString(cr.Object, "spec", "providerSpec", "kind")
	if kind != "GCPProviderSpec" {
		t.Errorf("providerSpec kind = %q, want GCPProviderSpec", kind)
	}
}

// TestResolveCredentials_RequestsOnlyThePermissionsItUses guards against a
// request quietly widening: every permission asked for should be one the
// operator actually calls.
func TestResolveCredentials_RequestsOnlyThePermissionsItUses(t *testing.T) {
	noAmbient(t)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	_, _ = ResolveCredentials(context.Background(), c, testNamespace)

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err != nil {
		t.Fatalf("the request was not raised: %v", err)
	}
	got, _, _ := unstructured.NestedStringSlice(cr.Object, "spec", "providerSpec", "permissions")
	if len(got) == 0 {
		t.Fatal("the request asked for no permissions")
	}
	for _, p := range got {
		if !slices.Contains(permissions, p) {
			t.Errorf("requested %q, which is not in the operator's own list", p)
		}
	}
}

// TestResolveCredentials_UsesTheMintedServiceAccount pins that a secret
// written by the cloud credential operator is used in preference to
// anything else.
func TestResolveCredentials_UsesTheMintedServiceAccount(t *testing.T) {
	noAmbient(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()

	got, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no client options returned")
	}
}

// TestResolveCredentials_WorkloadIdentityWinsOverTheServiceAccount pins the
// priority cloud-network-config-controller uses: a federated config where
// there is one, a service account key where there is not.
func TestResolveCredentials_WorkloadIdentityWinsOverTheServiceAccount(t *testing.T) {
	noAmbient(t)
	wif := `{"type":"external_account","universe_domain":"googleapis.com"}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data: map[string][]byte{
			secretServiceAccountKey:   []byte(serviceAccountJSON),
			secretWorkloadIdentityKey: []byte(wif),
		},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()

	raw, err := credentialsJSONFromSecret(secret)
	if err != nil {
		t.Fatalf("credentialsJSONFromSecret: %v", err)
	}
	var parsed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if parsed.Type != "external_account" {
		t.Errorf("credential type = %q, want external_account: the federated config must win", parsed.Type)
	}
	if _, err := ResolveCredentials(context.Background(), c, testNamespace); err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
}

// TestCredentialsJSON_SetsUniverseDomain is the trap
// cloud-network-config-controller documents: with no universe_domain the
// client goes to the metadata server to ask for one, and no pod here can
// reach 169.254.169.254.
func TestCredentialsJSON_SetsUniverseDomain(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}
	raw, err := credentialsJSONFromSecret(secret)
	if err != nil {
		t.Fatalf("credentialsJSONFromSecret: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if parsed["universe_domain"] != defaultUniverseDomain {
		t.Errorf("universe_domain = %v, want %q", parsed["universe_domain"], defaultUniverseDomain)
	}
}

// TestResolveCredentials_SecretWithNothingUsableIsAnError pins that a
// secret nobody can authenticate with is reported rather than passed on to
// fail somewhere less obvious.
func TestResolveCredentials_SecretWithNothingUsableIsAnError(t *testing.T) {
	noAmbient(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"unrelated": []byte("x")},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()

	if _, err := ResolveCredentials(context.Background(), c, testNamespace); err == nil {
		t.Fatal("expected an error for a secret with no usable credential")
	}
}

// TestResolveCredentials_KeepsTheRequestCurrentWhileUsingTheSecret pins
// that a request left behind by an older build is brought up to date.
//
// Observed on a live cluster: the permission list grew, the secret
// already existed, so nothing ever rewrote the request and the operator
// kept failing with a 403 for a permission it was now asking for in code
// and not in the cluster.
func TestResolveCredentials_KeepsTheRequestCurrentWhileUsingTheSecret(t *testing.T) {
	noAmbient(t)
	stale := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":      CredentialsRequestName,
			"namespace": CredentialsRequestNamespace,
		},
		"spec": map[string]any{
			"providerSpec": map[string]any{
				"apiVersion":  "cloudcredential.openshift.io/v1",
				"kind":        "GCPProviderSpec",
				"permissions": []any{"compute.routers.get"},
			},
		},
	}}
	stale.SetGroupVersionKind(CredentialsRequestGVK)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).
		WithObjects(secret, stale).Build()

	if _, err := ResolveCredentials(context.Background(), c, testNamespace); err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, got); err != nil {
		t.Fatalf("reading the request back: %v", err)
	}
	perms, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "providerSpec", "permissions")
	if len(perms) != len(permissions) {
		t.Errorf("the request still asks for %d permissions, want %d: a stale request is never corrected",
			len(perms), len(permissions))
	}
}

// TestResolveCredentials_AmbientLeavesTheClusterAlone pins the promise that
// a manager run from a desk creates nothing.
func TestResolveCredentials_AmbientLeavesTheClusterAlone(t *testing.T) {
	previous := ambientCredentials
	ambientCredentials = func(context.Context) ([]byte, error) {
		return []byte(serviceAccountJSON), nil
	}
	t.Cleanup(func() { ambientCredentials = previous })

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace); err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr)
	if err == nil {
		t.Error("a request was raised even though the process already had a credential")
	}
}

var _ client.Client = (client.Client)(nil)
