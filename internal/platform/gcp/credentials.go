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
	"fmt"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// CredentialsRequestGVK is the cloud credential operator's API, reached
// unstructured so that this operator does not vendor its types.
var CredentialsRequestGVK = schema.GroupVersionKind{
	Group: "cloudcredential.openshift.io", Version: "v1", Kind: "CredentialsRequest",
}

const (
	// CredentialsRequestName is the request the operator makes for itself.
	CredentialsRequestName = "bgp-cloud-connector-gcp"
	// CredentialsRequestNamespace is where the cloud credential operator
	// watches for them.
	CredentialsRequestNamespace = "openshift-cloud-credential-operator"
	// CredentialsSecretName is where it writes the answer, in this
	// operator's own namespace.
	CredentialsSecretName = "bgp-cloud-connector-gcp-credentials"
	// ServiceAccountName is the account the request is tied to.
	ServiceAccountName = "openshift-bgp-cloud-connector-controller-manager"
)

const (
	// The two keys the cloud credential operator writes, in the priority
	// cloud-network-config-controller reads them: a federated config
	// where the cluster uses workload identity, a service account key
	// where it mints.
	secretWorkloadIdentityKey = "workload_identity_config.json"
	secretServiceAccountKey   = "service_account.json"

	// defaultUniverseDomain is set explicitly on every credential.
	//
	// Without it the client asks the metadata server which universe it
	// is in, and no pod here can reach 169.254.169.254 -- measured on an
	// IPI cluster, where discovery failed with "dial tcp
	// 169.254.169.254:80: connect: connection refused" before a
	// credential was ever supplied. cloud-network-config-controller
	// carries the same workaround for the same reason.
	defaultUniverseDomain = "googleapis.com"
)

// permissions is what the operator actually calls, and nothing else. It
// reads the Cloud Router's interfaces, rewrites its BGP peers, reads and
// rewrites the NCC spoke that carries the router nodes, and sets
// canIpForward on those instances.
var permissions = []string{
	// Reading the Cloud Router's interfaces, and rewriting its peers.
	"compute.routers.get",
	"compute.routers.update",
	// Rewriting a Cloud Router's BGP peers is a change to the network's
	// policy as far as IAM is concerned, not just to the router:
	// measured, a 403 for compute.networks.updatePolicy on the cluster's
	// own network. No other operator here asks for it, because none of
	// them touch router peers.
	"compute.networks.updatePolicy",
	// Creating the router appliance spoke fetches the network the
	// instances are in, and fails with code=7 on "failed to fetch
	// resource .../global/networks/<name>" without this. It only shows
	// up when the operator creates the spoke rather than adopting one
	// somebody else made.
	"compute.networks.get",
	// canIpForward and nested virtualisation are both whole-instance
	// updates, not interface ones: compute.instances.updateNetworkInterface
	// is not enough, which a cluster said with a 403 rather than a
	// reading of the docs.
	"compute.instances.get",
	"compute.instances.update",
	// Every mutation returns a long-running operation that has to be
	// polled, and the two API families name that permission
	// separately: compute has zone and region operations,
	// networkconnectivity has its own. Missing the latter is not a
	// failure to create -- the spoke is made and then the poll is
	// refused, so the reconcile errors while the resource quietly
	// appears, and the next pass reports complete. Measured.
	"compute.zoneOperations.get",
	"compute.regionOperations.get",
	"networkconnectivity.operations.get",
	// The NCC spoke carrying the router nodes, which is created,
	// listed, patched and removed.
	"networkconnectivity.hubs.get",
	"networkconnectivity.spokes.get",
	"networkconnectivity.spokes.list",
	"networkconnectivity.spokes.create",
	"networkconnectivity.spokes.update",
	"networkconnectivity.spokes.delete",
}

// ambientCredentials is the chain the Google libraries would use on their
// own. Replaced in tests, and useful in exactly one real case: a manager
// run from a desk against an ordinary gcloud login.
var ambientCredentials = func(ctx context.Context) ([]byte, error) {
	creds, err := google.FindDefaultCredentials(ctx, computeScope)
	if err != nil {
		return nil, err
	}
	if len(creds.JSON) == 0 {
		// A credential with no JSON behind it is the metadata server,
		// which is what this operator cannot reach.
		return nil, fmt.Errorf("the default credential carries no JSON, so it is the metadata server")
	}
	return creds.JSON, nil
}

// computeScope is the one scope every call this operator makes needs.
const computeScope = "https://www.googleapis.com/auth/cloud-platform"

// ResolveCredentials returns the client options every GCP service here is
// built with.
//
// The order is cloud-network-config-controller's, because a cluster can be
// in any of these states and it has met all three: a federated config
// where workload identity is configured, a service account key where the
// cloud credential operator mints one, and whatever the process already
// has when somebody runs the manager from a desk.
//
// Where none of those exist the request is raised and ErrCredentialsPending
// is returned, which Reconcile waits out rather than treating as a fault:
// the secret appears a moment later and the next reconcile finds it.
func ResolveCredentials(ctx context.Context, c client.Client, namespace string) ([]option.ClientOption, error) {
	logger := log.FromContext(ctx)

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: CredentialsSecretName, Namespace: namespace}, secret)
	switch {
	case err == nil:
		raw, err := credentialsJSONFromSecret(secret)
		if err != nil {
			return nil, err
		}
		// Keep asking for what this build needs, not what an older one
		// did. The request is desired state: leaving it alone once the
		// secret exists means a permission added in code never reaches
		// the cluster, and the operator fails with a 403 for something
		// it believes it asked for. Observed on a live cluster.
		if err := reconcileCredentialsRequest(ctx, c, namespace); err != nil {
			return nil, err
		}
		logger.V(1).Info("using the credential provided by the cloud credential operator",
			"secret", CredentialsSecretName)
		return optionsFor(raw)
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, CredentialsSecretName, err)
	}

	if raw, err := ambientCredentials(ctx); err == nil {
		logger.V(1).Info("using the credential already available to this process")
		return optionsFor(raw)
	}

	if err := reconcileCredentialsRequest(ctx, c, namespace); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: secret %s/%s has not been written yet",
		platform.ErrCredentialsPending, namespace, CredentialsSecretName)
}

// credentialsJSONFromSecret picks the credential the secret describes and
// makes sure it names its universe, which the caller would otherwise have
// to go to the metadata server to learn.
func credentialsJSONFromSecret(secret *corev1.Secret) ([]byte, error) {
	for _, key := range []string{secretWorkloadIdentityKey, secretServiceAccountKey} {
		if raw, ok := secret.Data[key]; ok && len(raw) > 0 {
			return withUniverseDomain(raw)
		}
	}
	return nil, fmt.Errorf("secret %s/%s has neither %q nor %q; the cloud credential operator writes one of them in every mode, so this secret was not written by it",
		secret.Namespace, secret.Name, secretWorkloadIdentityKey, secretServiceAccountKey)
}

// withUniverseDomain returns the credential with universe_domain set,
// leaving one that already names it alone.
func withUniverseDomain(raw []byte) ([]byte, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("the credential is not JSON: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("the credential is not a JSON object")
	}
	if _, named := fields["universe_domain"]; named {
		return raw, nil
	}
	fields["universe_domain"] = defaultUniverseDomain
	return json.Marshal(fields)
}

// optionsFor names the credential type rather than leaving the library to
// infer it, which is the rule the Azure side follows too: which credential
// is in use should be readable here, not deduced at run time.
func optionsFor(raw []byte) ([]option.ClientOption, error) {
	var parsed struct {
		Type           string `json:"type"`
		UniverseDomain string `json:"universe_domain"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("the credential is not JSON: %w", err)
	}
	if parsed.Type == "" {
		return nil, fmt.Errorf("the credential names no type")
	}
	universe := parsed.UniverseDomain
	if universe == "" {
		universe = defaultUniverseDomain
	}
	return []option.ClientOption{
		option.WithAuthCredentialsJSON(option.CredentialsType(parsed.Type), raw),
		option.WithUniverseDomain(universe),
		option.WithScopes(computeScope),
	}, nil
}

// reconcileCredentialsRequest asks for the credential, and keeps asking
// for the same thing: a request that has drifted is rewritten rather than
// left, so the permissions in this file are the ones in the cluster.
func reconcileCredentialsRequest(ctx context.Context, c client.Client, namespace string) error {
	desired := desiredCredentialsRequest(namespace)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(CredentialsRequestGVK)
	err := c.Get(ctx, types.NamespacedName{
		Name: CredentialsRequestName, Namespace: CredentialsRequestNamespace,
	}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating the credentials request: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the credentials request: %w", err)
	}

	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	if equalSpecs(existingSpec, desiredSpec) {
		return nil
	}
	if err := unstructured.SetNestedMap(existing.Object, desiredSpec, "spec"); err != nil {
		return fmt.Errorf("rewriting the credentials request: %w", err)
	}
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating the credentials request: %w", err)
	}
	return nil
}

func equalSpecs(a, b map[string]any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}

func desiredCredentialsRequest(namespace string) *unstructured.Unstructured {
	perms := make([]any, 0, len(permissions))
	for _, p := range permissions {
		perms = append(perms, p)
	}
	cr := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name":      CredentialsRequestName,
			"namespace": CredentialsRequestNamespace,
		},
		"spec": map[string]any{
			"serviceAccountNames": []any{ServiceAccountName},
			"secretRef": map[string]any{
				"name":      CredentialsSecretName,
				"namespace": namespace,
			},
			"providerSpec": map[string]any{
				"apiVersion":  "cloudcredential.openshift.io/v1",
				"kind":        "GCPProviderSpec",
				"permissions": perms,
				// The herd sets this: the check asks whether an API is
				// enabled on the project, which the operator's own calls
				// report more usefully when they fail.
				"skipServiceCheck": true,
			},
		},
	}}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	return cr
}
