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

package azure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
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

// stubCredential stands in for whatever the SDK's own chain would find.
type stubCredential struct{}

func (stubCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, nil
}

// withAmbient replaces the chain probe for one test. A nil credential
// means this process has none of its own, which is the pod case.
func withAmbient(t *testing.T, cred azcore.TokenCredential) {
	t.Helper()
	previous := ambientCredential
	ambientCredential = func() (azcore.TokenCredential, error) {
		if cred == nil {
			return nil, errors.New("no credential available to this process")
		}
		return cred, nil
	}
	t.Cleanup(func() { ambientCredential = previous })
}

// withValidation replaces the token retrieval, so a test can say the
// credential is refused without needing a real Azure to refuse it.
func withValidation(t *testing.T, err error) {
	t.Helper()
	previous := validateCredential
	validateCredential = func(context.Context, azcore.TokenCredential) error { return err }
	t.Cleanup(func() { validateCredential = previous })
}

// tokenFile writes a file for WorkloadIdentityCredential to point at.
func tokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("a.b.c"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}
	return path
}

// The two shapes CCO writes. Which one you get says what mode the
// cluster is in, and naming the credential from that shape is the
// decision cloud-network-config-controller and cluster-ingress-operator
// both make.
func mintedSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data: map[string][]byte{
			"azure_client_id":       []byte("client-id"),
			"azure_client_secret":   []byte("client-secret"),
			"azure_tenant_id":       []byte("tenant-id"),
			"azure_subscription_id": []byte("subscription-id"),
			"azure_region":          []byte("centralus"),
		},
	}
}

func federatedSecret(tokenPath string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data: map[string][]byte{
			"azure_client_id":            []byte("client-id"),
			"azure_tenant_id":            []byte("tenant-id"),
			"azure_subscription_id":      []byte("subscription-id"),
			"azure_region":               []byte("centralus"),
			"azure_federated_token_file": []byte(tokenPath),
		},
	}
}

func getCredentialsRequest(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err != nil {
		t.Fatalf("reading the CredentialsRequest: %v", err)
	}
	return cr
}

// A manager run from a desk has a credential of its own and no secret,
// so it uses that and asks the cluster for nothing. This is the
// development loop, and it is the behaviour most worth protecting.
func TestResolveCredentials_AmbientWinsWhenThereIsNoSecret(t *testing.T) {
	withAmbient(t, stubCredential{})
	withValidation(t, nil)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	cred, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if _, ok := cred.(stubCredential); !ok {
		t.Errorf("got %T, want the credential this process already had", cred)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err == nil {
		t.Error("a CredentialsRequest was created even though this process already had a credential")
	}
}

// A pod on its first reconcile has neither, so it asks and waits.
func TestResolveCredentials_CreatesRequestAndWaits(t *testing.T) {
	withAmbient(t, nil)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace)
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}
	if !strings.Contains(err.Error(), CredentialsSecretName) {
		t.Errorf("the wait does not name the secret it is waiting for: %v", err)
	}

	cr := getCredentialsRequest(t, c)
	secretName, _, _ := unstructured.NestedString(cr.Object, "spec", "secretRef", "name")
	secretNS, _, _ := unstructured.NestedString(cr.Object, "spec", "secretRef", "namespace")
	if secretName != CredentialsSecretName || secretNS != testNamespace {
		t.Errorf("secretRef = %s/%s, want %s/%s", secretNS, secretName, testNamespace, CredentialsSecretName)
	}
	kind, _, _ := unstructured.NestedString(cr.Object, "spec", "providerSpec", "kind")
	if kind != "AzureProviderSpec" {
		t.Errorf("providerSpec kind = %q, want AzureProviderSpec", kind)
	}
	sas, _, _ := unstructured.NestedStringSlice(cr.Object, "spec", "serviceAccountNames")
	if !slices.Contains(sas, ServiceAccountName) {
		t.Errorf("serviceAccountNames = %v, want it to contain %q", sas, ServiceAccountName)
	}
}

// The permission list is the operator's whole cloud surface, so it is
// asserted exactly. In particular there is no compute permission: the
// operator never calls a compute API, and
// cloud-network-config-controller's list, the obvious one to copy, does
// ask for one.
func TestResolveCredentials_RequestsOnlyThePermissionsItUses(t *testing.T) {
	withAmbient(t, nil)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	_, _ = ResolveCredentials(context.Background(), c, testNamespace)

	cr := getCredentialsRequest(t, c)
	got, _, err := unstructured.NestedStringSlice(cr.Object, "spec", "providerSpec", "permissions")
	if err != nil {
		t.Fatalf("reading permissions: %v", err)
	}
	want := []string{
		"Microsoft.Network/virtualHubs/read",
		"Microsoft.Network/virtualHubs/bgpConnections/read",
		"Microsoft.Network/virtualHubs/bgpConnections/write",
		"Microsoft.Network/virtualHubs/bgpConnections/delete",
		"Microsoft.Network/networkInterfaces/read",
		"Microsoft.Network/networkInterfaces/write",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("permissions:\n got  %v\n want %v", got, want)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "Microsoft.Compute/") {
			t.Errorf("asked for a compute permission %q; the operator never calls a compute API", p)
		}
	}
}

// A cluster that mints or passes through gives a client secret.
func TestResolveCredentials_MintedSecretNamesAClientSecretCredential(t *testing.T) {
	withAmbient(t, stubCredential{})
	withValidation(t, nil)
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(mintedSecret()).
		Build()

	cred, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if _, ok := cred.(*azidentity.ClientSecretCredential); !ok {
		t.Errorf("got %T, want *azidentity.ClientSecretCredential", cred)
	}
}

// A cluster that federates gives a token file and no client secret. The
// absence of the secret is what decides it, which is why the fixture
// carries none rather than an empty one.
func TestResolveCredentials_FederatedSecretNamesAWorkloadIdentityCredential(t *testing.T) {
	withAmbient(t, stubCredential{})
	withValidation(t, nil)
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(federatedSecret(tokenFile(t))).
		Build()

	cred, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if _, ok := cred.(*azidentity.WorkloadIdentityCredential); !ok {
		t.Errorf("got %T, want *azidentity.WorkloadIdentityCredential", cred)
	}
}

// The secret wins over whatever this process happens to have. Where the
// cluster has provided a credential, that is the one the operator is
// meant to use.
func TestResolveCredentials_SecretWinsOverAmbient(t *testing.T) {
	withAmbient(t, stubCredential{})
	withValidation(t, nil)
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(mintedSecret()).
		Build()

	cred, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if _, ok := cred.(stubCredential); ok {
		t.Error("used the ambient credential while the cluster had provided one")
	}
}

// Reading the secret on every resolve is what makes a rotation take
// effect. An earlier design set process environment and probed it
// first, which answered itself on the second call and pinned the first
// credential for the life of the process.
func TestResolveCredentials_RotationTakesEffect(t *testing.T) {
	withAmbient(t, nil)
	withValidation(t, nil)
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(mintedSecret()).
		Build()

	first, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if _, ok := first.(*azidentity.ClientSecretCredential); !ok {
		t.Fatalf("first resolve gave %T", first)
	}

	if err := c.Delete(context.Background(), mintedSecret()); err != nil {
		t.Fatalf("removing the old secret: %v", err)
	}
	if err := c.Create(context.Background(), federatedSecret(tokenFile(t))); err != nil {
		t.Fatalf("writing the rotated secret: %v", err)
	}

	second, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if _, ok := second.(*azidentity.WorkloadIdentityCredential); !ok {
		t.Errorf("second resolve gave %T, want the rotated secret's credential", second)
	}
}

// A secret written by something other than CCO must not be treated as a
// credential.
func TestResolveCredentials_SecretMissingClientIDIsAnError(t *testing.T) {
	withAmbient(t, nil)
	secret := mintedSecret()
	delete(secret.Data, "azure_client_id")
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(secret).
		Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err == nil {
		t.Fatal("ResolveCredentials accepted a secret with no azure_client_id")
	}
	if !strings.Contains(err.Error(), "azure_client_id") {
		t.Errorf("the error does not name the missing key: %v", err)
	}
}

// Neither a client secret nor a token file means there is nothing to
// authenticate with, which is a different fault from a missing field and
// should say so.
func TestResolveCredentials_SecretWithNoWayToAuthenticate(t *testing.T) {
	withAmbient(t, nil)
	secret := mintedSecret()
	delete(secret.Data, "azure_client_secret")
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(secret).
		Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace)
	if err == nil {
		t.Fatal("ResolveCredentials accepted a secret carrying no means of authentication")
	}
	if !strings.Contains(err.Error(), "azure_federated_token_file") {
		t.Errorf("the error does not say what was missing: %v", err)
	}
}

// The credential is there and is refused. That is a fault the controller
// reports as CloudCredentialsInvalid, not a wait it sits out. Without
// asking for a token this arrives later from DiscoverEndpoints and reads
// as a discovery problem.
func TestResolveCredentials_RefusedTokenIsACredentialError(t *testing.T) {
	withAmbient(t, nil)
	withValidation(t, errors.New("AADSTS7000215: Invalid client secret provided"))
	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(mintedSecret()).
		Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace)
	var credErr *platform.CredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("ResolveCredentials: got %v, want a *platform.CredentialError", err)
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Errorf("the error does not repeat what Azure said: %v", err)
	}
}

// The request is reconciled rather than created once, so a drifted one
// is brought back.
func TestResolveCredentials_UpdatesADriftedRequest(t *testing.T) {
	withAmbient(t, nil)

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"secretRef": map[string]interface{}{
				"name": CredentialsSecretName, "namespace": testNamespace,
			},
			"providerSpec": map[string]interface{}{
				"apiVersion":  "cloudcredential.openshift.io/v1",
				"kind":        "AzureProviderSpec",
				"permissions": []interface{}{"Microsoft.Network/virtualHubs/read"},
			},
		},
	}}
	existing.SetGroupVersionKind(CredentialsRequestGVK)
	existing.SetName(CredentialsRequestName)
	existing.SetNamespace(CredentialsRequestNamespace)

	c := fake.NewClientBuilder().
		WithScheme(credentialsTestScheme(t)).
		WithObjects(existing).
		Build()
	_, _ = ResolveCredentials(context.Background(), c, testNamespace)

	cr := getCredentialsRequest(t, c)
	got, _, _ := unstructured.NestedStringSlice(cr.Object, "spec", "providerSpec", "permissions")
	if len(got) != 6 {
		t.Errorf("permissions = %v, want the full six restored", got)
	}
}
