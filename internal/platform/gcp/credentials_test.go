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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const testNamespace = "openshift-bgp-cloud-connector"

// testOwner stands in for the singleton BGPCloudConfiguration the
// controller passes in, which is the owner every request this operator
// makes is expected to carry.
func testOwner() metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "networking.openshift.io/v1beta1",
		Kind:       "BGPCloudConfiguration",
		Name:       "cluster",
		UID:        "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		Controller: &controller,
	}
}

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
// because the metadata server is unreachable, and whose Subscription
// names no workload identity whatever the shell running the tests has
// exported.
func noAmbient(t *testing.T) {
	t.Helper()
	withWorkloadIdentityEnv(t, nil)
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

	_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
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
	_, _ = ResolveCredentials(context.Background(), c, testNamespace, testOwner())

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
// anything else. The ambient credential is there and would work, as the
// node's service account does on a GCP pod, so a lookup that reached it
// first would pass unnoticed if the test only checked for a result.
func TestResolveCredentials_UsesTheMintedServiceAccount(t *testing.T) {
	withWorkloadIdentityEnv(t, nil)
	ambientCalled := false
	previous := ambientCredentials
	ambientCredentials = func(context.Context) ([]byte, error) {
		ambientCalled = true
		return []byte(serviceAccountJSON), nil
	}
	t.Cleanup(func() { ambientCredentials = previous })
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()

	got, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
	if err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no client options returned")
	}
	if ambientCalled {
		t.Error("the ambient credential was consulted although the secret exists")
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
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err != nil {
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

	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err == nil {
		t.Fatal("expected an error for a secret with no usable credential")
	}
}

// A secret the operator cannot use is a CredentialError, which the
// controller reports as CloudCredentialsInvalid, the reason the docs tell
// you to look for. Each way a secret can be unusable is covered.
func TestResolveCredentials_UnusableSecretIsACredentialError(t *testing.T) {
	for name, data := range map[string]map[string][]byte{
		"no key the operator reads": {"unrelated": []byte("x")},
		"not JSON":                  {secretServiceAccountKey: []byte("not json")},
		"no type":                   {secretServiceAccountKey: []byte(`{"client_email":"a@b"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			noAmbient(t)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
				Data:       data,
			}
			c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()
			_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
			var credErr *platform.CredentialError
			if !errors.As(err, &credErr) {
				t.Fatalf("ResolveCredentials: got %v (%T), want a *platform.CredentialError", err, err)
			}
		})
	}
}

// Two reconciles can both find no request and both create one. The one
// that loses gets AlreadyExists, which means the request is there, as
// on AWS and Azure, not that something failed.
func TestResolveCredentials_RequestCreatedConcurrentlyIsNotAnError(t *testing.T) {
	noAmbient(t)
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				return apierrors.NewAlreadyExists(schema.GroupResource{
					Group: CredentialsRequestGVK.Group, Resource: "credentialsrequests",
				}, obj.GetName())
			},
		}).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want the wait for the secret", err)
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

	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err != nil {
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
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err != nil {
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

func getCredentialsRequest(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err != nil {
		t.Fatalf("reading the request back: %v", err)
	}
	return cr
}

// The request must name the configuration as its owner, so that
// deleting the configuration takes the request with it and, on a
// cluster that mints, the service account CCO made for it. Without an
// owner the object outlives everything that referred to it.
func TestResolveCredentials_RequestIsOwnedByTheConfiguration(t *testing.T) {
	noAmbient(t)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	refs := getCredentialsRequest(t, c).GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("ownerReferences = %v, want exactly one", refs)
	}
	if got, want := refs[0], testOwner(); got.APIVersion != want.APIVersion ||
		got.Kind != want.Kind || got.Name != want.Name || got.UID != want.UID {
		t.Errorf("ownerReference = %+v, want %+v", got, want)
	}
}

// A request made by an earlier release has no owner, and there is
// nothing else in the system that will ever give it one. Reconciling
// has to adopt it, or the fix only reaches clusters installed after it.
// The secret is in place here, because that is the state such a
// cluster is in: the request was served long ago.
func TestResolveCredentials_ExistingRequestIsAdopted(t *testing.T) {
	noAmbient(t)

	unowned := desiredCredentialsRequest(testNamespace, metav1.OwnerReference{}, nil)
	unowned.SetOwnerReferences(nil)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(unowned, secret).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err != nil {
		t.Fatalf("ResolveCredentials: %v", err)
	}

	refs := getCredentialsRequest(t, c).GetOwnerReferences()
	if len(refs) != 1 || refs[0].UID != testOwner().UID {
		t.Errorf("ownerReferences = %v, want the configuration adopted", refs)
	}
}

// withWorkloadIdentityEnv sets the four variables the OperatorHub console
// writes into the Subscription on a cluster using workload identity
// federation, and clears any it is not given, so a test sees exactly
// what it names and nothing from the environment it runs in.
func withWorkloadIdentityEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{projectNumberEnvVar, poolIDEnvVar, providerIDEnvVar, serviceAccountEmailEnvVar} {
		t.Setenv(name, env[name])
	}
}

func fullWorkloadIdentityEnv() map[string]string {
	return map[string]string{
		projectNumberEnvVar:       "123456789012",
		poolIDEnvVar:              "my-pool",
		providerIDEnvVar:          "my-provider",
		serviceAccountEmailEnvVar: "bgpcc@my-project.iam.gserviceaccount.com",
	}
}

const wantAudience = "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-pool/providers/my-provider"

// Without the console's variables the request must carry none of the
// federation fields. The cloud credential operator mints for a request
// without them, and on a cluster that mints a token path is a request
// for something it cannot do.
func TestResolveCredentials_RequestOmitsWorkloadIdentityFieldsWithoutEnv(t *testing.T) {
	noAmbient(t)
	withWorkloadIdentityEnv(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	cr := getCredentialsRequest(t, c)
	for _, path := range [][]string{
		{"spec", "providerSpec", "serviceAccountEmail"},
		{"spec", "providerSpec", "audience"},
		{"spec", "cloudTokenPath"},
	} {
		if v, found, _ := unstructured.NestedString(cr.Object, path...); found {
			t.Errorf("%s was set to %q with no workload identity in the environment", strings.Join(path, "."), v)
		}
	}
}

// With the console's variables the request must carry the service
// account, the audience in the form the cloud credential operator
// validates, and the token path. On a cluster using workload identity
// federation it writes no secret for a request without a service
// account.
func TestResolveCredentials_RequestCarriesWorkloadIdentityFromEnv(t *testing.T) {
	noAmbient(t)
	withWorkloadIdentityEnv(t, fullWorkloadIdentityEnv())

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	cr := getCredentialsRequest(t, c)
	for _, tc := range []struct {
		path []string
		want string
	}{
		{[]string{"spec", "providerSpec", "serviceAccountEmail"}, "bgpcc@my-project.iam.gserviceaccount.com"},
		{[]string{"spec", "providerSpec", "audience"}, wantAudience},
		{[]string{"spec", "cloudTokenPath"}, cloudTokenPath},
	} {
		if got, _, _ := unstructured.NestedString(cr.Object, tc.path...); got != tc.want {
			t.Errorf("%s = %q, want %q", strings.Join(tc.path, "."), got, tc.want)
		}
	}
}

// The variables arrive by editing the Subscription, which can happen
// after the operator has made its request. A request that is never
// rewritten would leave the operator waiting on a secret the cloud
// credential operator has no reason to write.
func TestResolveCredentials_ExistingRequestGainsWorkloadIdentity(t *testing.T) {
	noAmbient(t)
	withWorkloadIdentityEnv(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("first resolve: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	withWorkloadIdentityEnv(t, fullWorkloadIdentityEnv())
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("second resolve: got %v, want %v", err, platform.ErrCredentialsPending)
	}

	if got, _, _ := unstructured.NestedString(getCredentialsRequest(t, c).Object, "spec", "providerSpec", "audience"); got != wantAudience {
		t.Errorf("audience = %q after the variables appeared, want %q", got, wantAudience)
	}
}

// The console leaves out any field the administrator left empty, so a
// partial set is a real input. A request built from it would be
// rejected by the cloud credential operator on its own status, where
// nobody looks; say which variable is missing here instead, and do not
// touch the request.
func TestResolveCredentials_PartialWorkloadIdentityIsAnError(t *testing.T) {
	noAmbient(t)
	env := fullWorkloadIdentityEnv()
	delete(env, providerIDEnvVar)
	withWorkloadIdentityEnv(t, env)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
	if err == nil || errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want an error that is not a wait", err)
	}
	if !strings.Contains(err.Error(), providerIDEnvVar) {
		t.Errorf("the error does not name %s: %q", providerIDEnvVar, err)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err == nil {
		t.Error("a request was raised from an incomplete workload identity")
	}
}

// A secret that already works is used even when the Subscription's
// workload identity is incomplete, and the request it was written for is
// left alone: an edit to the Subscription must not stop an operator that
// holds a usable credential, nor rewrite the request on the strength of
// an identity that cannot be sent.
func TestResolveCredentials_PartialWorkloadIdentityDoesNotStopAWorkingSecret(t *testing.T) {
	noAmbient(t)
	withWorkloadIdentityEnv(t, fullWorkloadIdentityEnv())
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{secretServiceAccountKey: []byte(serviceAccountJSON)},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner()); err != nil {
		t.Fatalf("ResolveCredentials with the full identity: %v", err)
	}
	before, _, _ := unstructured.NestedMap(getCredentialsRequest(t, c).Object, "spec")

	env := fullWorkloadIdentityEnv()
	delete(env, providerIDEnvVar)
	withWorkloadIdentityEnv(t, env)
	got, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
	if err != nil {
		t.Fatalf("ResolveCredentials with a partial identity and a working secret: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no client options returned")
	}
	after, _, _ := unstructured.NestedMap(getCredentialsRequest(t, c).Object, "spec")
	if !equalSpecs(before, after) {
		t.Errorf("the request was rewritten from an incomplete identity:\nbefore %v\nafter  %v", before, after)
	}
}

// The audience names the project by number. The cloud credential
// operator rejects anything else as malformed, again only on its own
// status, so a project id in the number's place is caught here.
func TestResolveCredentials_ProjectNumberMustBeANumber(t *testing.T) {
	noAmbient(t)
	env := fullWorkloadIdentityEnv()
	env[projectNumberEnvVar] = "my-project"
	withWorkloadIdentityEnv(t, env)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
	if err == nil || errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want an error that is not a wait", err)
	}
	if !strings.Contains(err.Error(), projectNumberEnvVar) {
		t.Errorf("the error does not name %s: %q", projectNumberEnvVar, err)
	}
}

// The pool and provider are path segments of the audience, and the
// cloud credential operator's pattern for it allows no slash in either.
// An id carrying one would produce an audience it rejects.
func TestResolveCredentials_IDsCannotSpanPathSegments(t *testing.T) {
	for _, name := range []string{poolIDEnvVar, providerIDEnvVar} {
		t.Run(name, func(t *testing.T) {
			noAmbient(t)
			env := fullWorkloadIdentityEnv()
			env[name] = "projects/1/locations/global/workloadIdentityPools/x"
			withWorkloadIdentityEnv(t, env)

			c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
			_, err := ResolveCredentials(context.Background(), c, testNamespace, testOwner())
			if err == nil || errors.Is(err, platform.ErrCredentialsPending) {
				t.Fatalf("ResolveCredentials: got %v, want an error that is not a wait", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the error does not name %s: %q", name, err)
			}
		})
	}
}

var _ client.Client = (client.Client)(nil)

// TestCredentialsJSON_UnusableSecretNamesTheKeyCCOWrites pins what the
// error tells you to look for: the cloud credential operator writes
// service_account.json in both modes, so that is the key a secret it
// wrote cannot be missing.
func TestCredentialsJSON_UnusableSecretNamesTheKeyCCOWrites(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"something-else": []byte(`{}`)},
	}
	_, err := credentialsJSONFromSecret(secret)
	if err == nil {
		t.Fatal("credentialsJSONFromSecret: want an error for a secret with neither key")
	}
	want := `the cloud credential operator writes "service_account.json" in every mode`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}
