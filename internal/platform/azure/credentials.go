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
	"fmt"
	"reflect"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// CredentialsRequestGVK is the cloud credential operator's API,
// addressed as unstructured for the same reason the AWS path does it:
// the typed package drags in a dependency tree out of all proportion to
// one object.
var CredentialsRequestGVK = schema.GroupVersionKind{
	Group: "cloudcredential.openshift.io", Version: "v1", Kind: "CredentialsRequest",
}

const (
	// CredentialsRequestName is the request the operator makes for itself.
	CredentialsRequestName = "bgp-cloud-connector-azure"

	// CredentialsRequestNamespace is where the cloud credential operator
	// looks for requests. It is not where the secret lands.
	CredentialsRequestNamespace = "openshift-cloud-credential-operator"

	// CredentialsSecretName is the secret CCO writes, in the operator's
	// own namespace.
	CredentialsSecretName = "bgp-cloud-connector-azure-credentials"

	// ServiceAccountName is the operator's ServiceAccount. On a cluster
	// that federates it is the subject the managed identity's federated
	// credential names.
	ServiceAccountName = "openshift-bgp-cloud-connector-controller-manager"

	// cloudTokenPath is where the deployment projects the bound
	// ServiceAccount token. ccoctl writes it into the secret as
	// azure_federated_token_file; CCO ignores it in the modes that do not
	// federate.
	cloudTokenPath = "/var/run/secrets/openshift/serviceaccount/token"

	// managementScope is what a token has to be good for. Every call this
	// operator makes is an ARM call.
	managementScope = "https://management.azure.com/.default"
)

// The keys CCO writes. Which of them are present is how the cluster's
// mode reaches this code: a client secret where it mints or passes
// through, a federated token file where it federates.
const (
	secretClientID           = "azure_client_id"
	secretTenantID           = "azure_tenant_id"
	secretClientSecret       = "azure_client_secret"
	secretFederatedTokenFile = "azure_federated_token_file"
)

// permissions is what the operator needs and no more: reading the Route
// Server, managing its BGP connections, and turning on IP forwarding on
// the router nodes' interfaces.
//
// There is deliberately no compute permission. The operator never calls
// a compute API: it parses the virtual machine out of the node's
// providerID and matches interfaces on their VirtualMachine.ID.
// openshift-cloud-network-config-controller-azure is the obvious list to
// copy and it does ask for Microsoft.Compute/virtualMachines/read, which
// would over-grant here.
var permissions = []string{
	"Microsoft.Network/virtualHubs/read",
	"Microsoft.Network/virtualHubs/bgpConnections/read",
	"Microsoft.Network/virtualHubs/bgpConnections/write",
	"Microsoft.Network/virtualHubs/bgpConnections/delete",
	"Microsoft.Network/networkInterfaces/read",
	"Microsoft.Network/networkInterfaces/write",
}

// validateCredential asks for a token, which is the only way to know
// whether a credential works.
//
// Constructing one proves nothing. azidentity.NewDefaultAzureCredential
// returns a credential and a nil error when nothing in its chain can
// produce a token at all, and NewClientSecretCredential accepts a secret
// it has never tried. Without this the failure surfaces later from
// DiscoverEndpoints and is reported as a discovery problem, which sends
// whoever reads it to the wrong subsystem.
//
// No other operator here does this. It is a deliberate divergence.
//
// Overridden in tests.
var validateCredential = func(ctx context.Context, cred azcore.TokenCredential) error {
	_, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{managementScope}})
	return err
}

// ambientCredential is whatever the SDK's own chain can find. On a
// manager run from a desk that is an az login; in a pod it is nothing,
// because the image carries no az and IMDS is unreachable from the pod
// network.
//
// Overridden in tests.
var ambientCredential = func() (azcore.TokenCredential, error) {
	return azidentity.NewDefaultAzureCredential(nil)
}

// ResolveCredentials returns the credential every Azure call should
// use, asking the cluster for one if it has to.
//
// The secret comes first, because where the cluster has provided a
// credential that is the one the operator is meant to use, and reading
// it each time is what makes a rotation take effect. Only when there is
// none does this fall back to the SDK's chain, which is what serves a
// manager run from a desk and is why that loop keeps working. A desk
// run therefore asks the cluster for nothing.
//
// The credential type is named rather than left to the chain, as every
// other operator here does it: which credential is in use should be
// readable from the code, the chain reaches IMDS and IMDS is
// unreachable from a pod, and spec.azure.networkInterfaceClientID means
// two identities can be in play at once, which one process-wide chain
// cannot express.
func ResolveCredentials(ctx context.Context, c client.Client, namespace string) (azcore.TokenCredential, error) {
	logger := log.FromContext(ctx)

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: CredentialsSecretName, Namespace: namespace}, secret)
	switch {
	case err == nil:
		cred, err := credentialFromSecret(secret)
		if err != nil {
			return nil, err
		}
		if err := validateCredential(ctx, cred); err != nil {
			return nil, &platform.CredentialError{
				Msg: fmt.Sprintf("the credential in secret %s/%s was refused: %v",
					namespace, CredentialsSecretName, err),
			}
		}
		logger.V(1).Info("using the credential provided by the cloud credential operator",
			"secret", CredentialsSecretName)
		return cred, nil
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, CredentialsSecretName, err)
	}

	if cred, err := ambientCredential(); err == nil {
		if err := validateCredential(ctx, cred); err == nil {
			logger.V(1).Info("using the credential already available to this process")
			return cred, nil
		}
	}

	if err := reconcileCredentialsRequest(ctx, c, namespace); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: secret %s/%s has not been written yet",
		platform.ErrCredentialsPending, namespace, CredentialsSecretName)
}

// credentialFromSecret names the credential the secret describes, which
// is the same decision cloud-network-config-controller and
// cluster-ingress-operator make: a client secret where there is one, a
// federated token file where there is not.
func credentialFromSecret(secret *corev1.Secret) (azcore.TokenCredential, error) {
	clientID := string(secret.Data[secretClientID])
	tenantID := string(secret.Data[secretTenantID])
	for name, value := range map[string]string{secretClientID: clientID, secretTenantID: tenantID} {
		if value == "" {
			return nil, fmt.Errorf("secret %s/%s has no %q; the cloud credential operator writes one in every mode, so this secret was not written by it",
				secret.Namespace, secret.Name, name)
		}
	}

	if clientSecret := string(secret.Data[secretClientSecret]); clientSecret != "" {
		cred, err := azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)
		if err != nil {
			return nil, &platform.CredentialError{Msg: fmt.Sprintf("Azure client secret credential: %v", err)}
		}
		return cred, nil
	}

	if tokenFile := string(secret.Data[secretFederatedTokenFile]); tokenFile != "" {
		cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID:      clientID,
			TenantID:      tenantID,
			TokenFilePath: tokenFile,
		})
		if err != nil {
			return nil, &platform.CredentialError{Msg: fmt.Sprintf("Azure workload identity credential: %v", err)}
		}
		return cred, nil
	}

	return nil, fmt.Errorf("secret %s/%s carries neither %q nor %q, so there is no way to authenticate with it",
		secret.Namespace, secret.Name, secretClientSecret, secretFederatedTokenFile)
}

// reconcileCredentialsRequest brings the request to what we want it to
// be, rather than creating it once. An administrator narrowing the
// permissions, or this operator widening them in a later release, would
// otherwise leave the cluster serving a request nobody wrote.
func reconcileCredentialsRequest(ctx context.Context, c client.Client, namespace string) error {
	desired := desiredCredentialsRequest(namespace)

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(CredentialsRequestGVK)
	err := c.Get(ctx, types.NamespacedName{
		Name:      CredentialsRequestName,
		Namespace: CredentialsRequestNamespace,
	}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("creating CredentialsRequest %s: %w", CredentialsRequestName, err)
		}
		log.FromContext(ctx).Info("asked the cloud credential operator for Azure credentials",
			"credentialsRequest", CredentialsRequestName, "secret", CredentialsSecretName)
		return nil
	case err != nil:
		return fmt.Errorf("reading CredentialsRequest %s: %w", CredentialsRequestName, err)
	}

	if reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		return nil
	}

	existing.Object["spec"] = desired.Object["spec"]
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating CredentialsRequest %s: %w", CredentialsRequestName, err)
	}
	log.FromContext(ctx).Info("updated the request for Azure credentials",
		"credentialsRequest", CredentialsRequestName)
	return nil
}

func desiredCredentialsRequest(namespace string) *unstructured.Unstructured {
	perms := make([]interface{}, 0, len(permissions))
	for _, p := range permissions {
		perms = append(perms, p)
	}

	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"secretRef": map[string]interface{}{
				"name":      CredentialsSecretName,
				"namespace": namespace,
			},
			"serviceAccountNames": []interface{}{ServiceAccountName},
			// Carried in every mode. CCO ignores it where it mints or
			// passes through, and ccoctl reads it on a cluster that
			// federates, where it becomes azure_federated_token_file in
			// the secret. Setting it unconditionally means the request an
			// administrator feeds to ccoctl is already right, rather than
			// depending on which cluster the operator first ran on.
			"cloudTokenPath": cloudTokenPath,
			"providerSpec": map[string]interface{}{
				"apiVersion":  "cloudcredential.openshift.io/v1",
				"kind":        "AzureProviderSpec",
				"permissions": perms,
			},
		},
	}}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	cr.SetName(CredentialsRequestName)
	cr.SetNamespace(CredentialsRequestNamespace)
	return cr
}
