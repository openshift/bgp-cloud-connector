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

package aws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
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

// The two shapes the cloud credential operator writes into the
// credentials key. Which one you get says which mode the cluster is in,
// and the point of reading that key rather than the individual ones is
// that the operator does not have to care.
const (
	mintedINI = `[default]
aws_access_key_id = AKIAMINTED
aws_secret_access_key = mintedsecret`

	stsINI = `[default]
sts_regional_endpoints = regional
role_arn = arn:aws:iam::123456789012:role/bgp-cloud-connector
web_identity_token_file = /var/run/secrets/openshift/serviceaccount/token`
)

func credentialsTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("registering the client-go scheme: %v", err)
	}
	s.AddKnownTypeWithName(CredentialsRequestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(CredentialsRequestGVK.GroupVersion().WithKind("CredentialsRequestList"), &unstructured.UnstructuredList{})
	return s
}

// nestedString reads a field out of the request and fails the test if the
// object is not the shape it expects. Left to return a zero value, an
// unexpected shape makes the assertions below compare against "" and pass
// on a field that was never read.
func nestedString(t *testing.T, obj map[string]interface{}, fields ...string) (string, bool) {
	t.Helper()
	v, found, err := unstructured.NestedString(obj, fields...)
	if err != nil {
		t.Fatalf("reading %s: %v", strings.Join(fields, "."), err)
	}
	return v, found
}

// resolvePending runs a resolve that is expected to report pending: these
// tests create no secret, so the operator makes its request and waits.
// Asserting the error keeps the assertions on the request below from
// passing on a run that failed for some entirely different reason.
func resolvePending(t *testing.T, c client.Client) {
	t.Helper()
	if _, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2"); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("ResolveCredentials: got %v, want %v", err, platform.ErrCredentialsPending)
	}
}

// withAmbient replaces the ambient credential probe for the duration of a
// test. A nil provider means the pod has no credentials of its own.
func withAmbient(t *testing.T, provider awssdk.CredentialsProvider) {
	t.Helper()
	previous := ambientCredentials
	ambientCredentials = func(_ context.Context, _ string) (awssdk.CredentialsProvider, error) {
		if provider == nil {
			return nil, errors.New("no ambient credentials")
		}
		return provider, nil
	}
	t.Cleanup(func() { ambientCredentials = previous })
}

// isolateTempDir gives a test its own directory for the credentials file,
// so tests that run in the same process do not read each other's.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func secretWithCredentials(ini string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{credentialsKey: []byte(ini)},
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

// On ROSA the pod identity webhook injects a web identity token, so the
// SDK's own chain answers and nothing should be asked of the cluster.
func TestResolveCredentials_AmbientChainWins(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, credentials.NewStaticCredentialsProvider("ambient", "secret", ""))

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	opts, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds := retrieve(t, opts)
	if creds.AccessKeyID != "ambient" {
		t.Errorf("expected the ambient credentials, got AccessKeyID %q", creds.AccessKeyID)
	}

	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, cr); err == nil {
		t.Error("a CredentialsRequest was created even though the pod already had credentials")
	}
}

// On IPI there is nothing in the pod, so the operator asks CCO and waits
// for the secret rather than reporting a fault.
func TestResolveCredentials_CreatesRequestAndWaits(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	if _, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2"); !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("expected platform.ErrCredentialsPending, got %v", err)
	}

	cr := getCredentialsRequest(t, c)

	name, _ := nestedString(t, cr.Object, "spec", "secretRef", "name")
	namespace, _ := nestedString(t, cr.Object, "spec", "secretRef", "namespace")
	if name != CredentialsSecretName || namespace != testNamespace {
		t.Errorf("secretRef points at %s/%s, want %s/%s", namespace, name, testNamespace, CredentialsSecretName)
	}

	entries, found, err := unstructured.NestedSlice(cr.Object, "spec", "providerSpec", "statementEntries")
	if err != nil || !found || len(entries) != 1 {
		t.Fatalf("expected one statement entry, got %v (found=%v, err=%v)", entries, found, err)
	}
	entry, ok := entries[0].(map[string]interface{})
	if !ok {
		t.Fatalf("statement entry is %T, want a map", entries[0])
	}
	actions, _, err := unstructured.NestedStringSlice(entry, "action")
	if err != nil {
		t.Fatalf("reading the actions: %v", err)
	}
	// The peer calls are the ones without which the operator can do
	// nothing at all; if the list is ever trimmed, it fails here.
	for _, want := range []string{"ec2:CreateRouteServerPeer", "ec2:DeleteRouteServerPeer", "ec2:ModifyNetworkInterfaceAttribute"} {
		if !slices.Contains(actions, want) {
			t.Errorf("the policy does not grant %s: %v", want, actions)
		}
	}
}

// Without a role ARN the request must carry neither STS field. CCO reads
// their absence as "mint me an IAM user", and a cloudTokenPath on a
// cluster that mints is a request for something it cannot do.
func TestResolveCredentials_RequestOmitsSTSFieldsWithoutRoleARN(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)
	t.Setenv(roleARNEnvVar, "")

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	resolvePending(t, c)

	cr := getCredentialsRequest(t, c)
	if arn, found := nestedString(t, cr.Object, "spec", "providerSpec", "stsIAMRoleARN"); found {
		t.Errorf("stsIAMRoleARN was set to %q with no role ARN in the environment", arn)
	}
	if path, found := nestedString(t, cr.Object, "spec", "cloudTokenPath"); found {
		t.Errorf("cloudTokenPath was set to %q with no role ARN in the environment", path)
	}
}

// With a role ARN -- which OLM sets from the Subscription on an STS
// cluster -- both fields must be present, or CCO decides the request is
// not addressed to it and creates nothing at all.
func TestResolveCredentials_RequestCarriesSTSFieldsWithRoleARN(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)
	const roleARN = "arn:aws:iam::123456789012:role/bgp-cloud-connector"
	t.Setenv(roleARNEnvVar, roleARN)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	resolvePending(t, c)

	cr := getCredentialsRequest(t, c)
	if got, _ := nestedString(t, cr.Object, "spec", "providerSpec", "stsIAMRoleARN"); got != roleARN {
		t.Errorf("stsIAMRoleARN is %q, want %q", got, roleARN)
	}
	if got, _ := nestedString(t, cr.Object, "spec", "cloudTokenPath"); got != cloudTokenPath {
		t.Errorf("cloudTokenPath is %q, want %q", got, cloudTokenPath)
	}
}

// The role ARN arrives by editing the Subscription, which happens after
// the operator has already made its request. A request that is only ever
// created and never updated would leave the operator waiting on a secret
// CCO has no reason to write.
func TestResolveCredentials_ExistingRequestGainsRoleARN(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()
	resolvePending(t, c)

	const roleARN = "arn:aws:iam::123456789012:role/bgp-cloud-connector"
	t.Setenv(roleARNEnvVar, roleARN)
	resolvePending(t, c)

	cr := getCredentialsRequest(t, c)
	if got, _ := nestedString(t, cr.Object, "spec", "providerSpec", "stsIAMRoleARN"); got != roleARN {
		t.Errorf("stsIAMRoleARN is %q after the role ARN appeared, want %q", got, roleARN)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(CredentialsRequestGVK.GroupVersion().WithKind("CredentialsRequestList"))
	if err := c.List(context.Background(), list, client.InNamespace(CredentialsRequestNamespace)); err != nil {
		t.Fatalf("listing credentials requests: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("expected exactly one CredentialsRequest, got %d", len(list.Items))
	}
}

// Mint and passthrough clusters put static keys in the credentials key.
func TestResolveCredentials_MintedSecret(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).
		WithObjects(secretWithCredentials(mintedINI)).Build()

	opts, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	creds := retrieve(t, opts)
	if creds.AccessKeyID != "AKIAMINTED" || creds.SecretAccessKey != "mintedsecret" {
		t.Errorf("got %q/%q, want AKIAMINTED/mintedsecret", creds.AccessKeyID, creds.SecretAccessKey)
	}
}

// An STS cluster puts a role and a token path in the same key. Retrieving
// would call STS, so this asserts the SDK parsed what we wrote: that is
// the whole claim, since the SDK owns everything after it.
func TestResolveCredentials_STSSecret(t *testing.T) {
	dir := isolateTempDir(t)
	withAmbient(t, nil)

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).
		WithObjects(secretWithCredentials(stsINI)).Build()

	if _, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	path := filepath.Join(dir, credentialsFileName)
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the credentials file the operator wrote: %v", err)
	}
	if string(written) != stsINI {
		t.Errorf("the credentials file does not match the secret:\n%s", written)
	}

	shared, err := config.LoadSharedConfigProfile(context.Background(), "default",
		func(o *config.LoadSharedConfigOptions) { o.CredentialsFiles = []string{path} })
	if err != nil {
		t.Fatalf("the SDK could not parse the file: %v", err)
	}
	if shared.RoleARN != "arn:aws:iam::123456789012:role/bgp-cloud-connector" {
		t.Errorf("RoleARN is %q", shared.RoleARN)
	}
	if shared.WebIdentityTokenFile != cloudTokenPath {
		t.Errorf("WebIdentityTokenFile is %q, want %q", shared.WebIdentityTokenFile, cloudTokenPath)
	}
}

// A secret that exists but carries nothing usable is a fault, not
// something to wait out: CCO has answered, and the answer is no good.
func TestResolveCredentials_SecretWithoutCredentialsKey(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"something_else": []byte("nonsense")},
	}
	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).WithObjects(secret).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2")
	if err == nil {
		t.Fatal("expected an error for a secret with no credentials key")
	}
	if errors.Is(err, platform.ErrCredentialsPending) {
		t.Error("a malformed secret should not read as still pending")
	}
}

func retrieve(t *testing.T, opts CredentialOptions) awssdk.Credentials {
	t.Helper()
	cfg, err := config.LoadDefaultConfig(context.Background(), append(opts, config.WithRegion("us-east-2"))...)
	if err != nil {
		t.Fatalf("loading the config the operator resolved: %v", err)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieving credentials: %v", err)
	}
	return creds
}

// The cloud credential operator ignores a request without stsIAMRoleARN
// on a cluster that federates, and reports it provisioned regardless, so
// the CredentialsRequest looks healthy and no secret ever appears. This
// condition is the only place an administrator will be told why, so it
// has to say what to do rather than imply the wait will end.
func TestResolveCredentials_PendingWithoutRoleARNNamesIt(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)
	t.Setenv(roleARNEnvVar, "")

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2")
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("expected platform.ErrCredentialsPending, got %v", err)
	}
	if !strings.Contains(err.Error(), roleARNEnvVar) {
		t.Errorf("the wait says nothing about %s, so an STS cluster waits forever with no clue: %q", roleARNEnvVar, err)
	}
}

// With a role ARN the wait is an ordinary one and will end, so it should
// not read as though something is missing.
func TestResolveCredentials_PendingWithRoleARNIsAnOrdinaryWait(t *testing.T) {
	isolateTempDir(t)
	withAmbient(t, nil)
	t.Setenv(roleARNEnvVar, "arn:aws:iam::123456789012:role/bgp-cloud-connector")

	c := fake.NewClientBuilder().WithScheme(credentialsTestScheme(t)).Build()

	_, err := ResolveCredentials(context.Background(), c, testNamespace, "us-east-2")
	if !errors.Is(err, platform.ErrCredentialsPending) {
		t.Fatalf("expected platform.ErrCredentialsPending, got %v", err)
	}
	if strings.Contains(err.Error(), "set "+roleARNEnvVar) {
		t.Errorf("the wait tells the administrator to set %s when it is already set: %q", roleARNEnvVar, err)
	}
	if !strings.Contains(err.Error(), CredentialsSecretName) {
		t.Errorf("the wait does not name the secret it is waiting for: %q", err)
	}
}

// TestWriteCredentialsFile_FailureLeavesNothingBehind covers the path where
// the rename cannot happen.
//
// The temporary holds AWS credentials and lives in a directory shared with
// every other process on the node, so failing to install it and failing to
// clear it up are two separate problems and the caller has to hear about
// both. Driven by making the destination a directory, which is the one way
// to fail the rename without breaking the filesystem underneath the test.
func TestWriteCredentialsFile_FailureLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	if err := os.Mkdir(filepath.Join(dir, credentialsFileName), 0o700); err != nil {
		t.Fatalf("setting up the blocked destination: %v", err)
	}

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the temp directory: %v", err)
	}

	if _, err := writeCredentialsFile([]byte("[default]\n")); err == nil {
		t.Fatal("expected an error when the destination cannot be renamed over")
	} else if !strings.Contains(err.Error(), "installing the credentials file") {
		t.Errorf("error should name the step that failed, got: %v", err)
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the temp directory: %v", err)
	}
	if len(after) != len(before) {
		names := make([]string, 0, len(after))
		for _, e := range after {
			names = append(names, e.Name())
		}
		t.Errorf("left %d entries behind, want %d: %v", len(after), len(before), names)
	}
}
