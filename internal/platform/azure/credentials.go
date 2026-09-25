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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
//
// Writing a router node's interface is also a request to join the two
// things it is attached to: its subnet and its load balancer's backend
// pool. ARM checks both as linked scopes and refuses the write without
// join/action on each, naming only the first one missing, so a list
// short of either looks like a list short of one.
//
// The modes that pass a credential through never read this list, which
// is why the omission stayed invisible: only a mode that mints a role
// from it, as ccoctl does, hands out what is written here.
var permissions = []string{
	"Microsoft.Network/virtualHubs/read",
	"Microsoft.Network/virtualHubs/bgpConnections/read",
	"Microsoft.Network/virtualHubs/bgpConnections/write",
	"Microsoft.Network/virtualHubs/bgpConnections/delete",
	"Microsoft.Network/networkInterfaces/read",
	"Microsoft.Network/networkInterfaces/write",
	"Microsoft.Network/virtualNetworks/subnets/join/action",
	"Microsoft.Network/loadBalancers/backendAddressPools/join/action",
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
// The request is reconciled on the secret path as well as without it,
// so it keeps up with the permissions this release asks for rather than
// standing at whatever created it.
//
// The credential type is named rather than left to the chain, as every
// other operator here does it: which credential is in use should be
// readable from the code, the chain reaches IMDS and IMDS is
// unreachable from a pod, and spec.azure.networkInterfaceClientID means
// two identities can be in play at once, which one process-wide chain
// cannot express.
func ResolveCredentials(ctx context.Context, c client.Client, namespace string, owner metav1.OwnerReference) (azcore.TokenCredential, error) {
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
		// Still reconciled, even though the credential is already in
		// hand. Once the secret exists this is the only path taken, so
		// returning here would freeze the request at whatever an
		// earlier release asked for: a permission added later would
		// never reach a cluster installed before it, and nothing would
		// ever repair that. A failure is returned rather than logged,
		// as on AWS -- the reconcile is requeued and tries again, and
		// an operator that cannot write its own request is worth
		// seeing.
		if err := reconcileCredentialsRequest(ctx, c, namespace, owner); err != nil {
			return nil, err
		}
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

	if err := reconcileCredentialsRequest(ctx, c, namespace, owner); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: secret %s/%s has not been written yet",
		platform.ErrCredentialsPending, namespace, CredentialsSecretName)
}

// credentialFromSecret names the credential the secret describes, which
// is the same decision cloud-network-config-controller and
// cluster-ingress-operator make: a client secret where there is one, a
// federated token file where there is not.
//
// A secret it cannot use is a CredentialError, as a refused token
// already is. Reconcile reads the type to choose between
// CloudCredentialsInvalid and CloudDiscoveryFailed, and a secret CCO
// did not write is not a discovery problem: reported as one it sends
// whoever reads it to the wrong subsystem. It also stops the requeue,
// which is right here -- nothing about waiting makes a secret grow the
// key it is missing.
func credentialFromSecret(secret *corev1.Secret) (azcore.TokenCredential, error) {
	clientID := string(secret.Data[secretClientID])
	tenantID := string(secret.Data[secretTenantID])
	for name, value := range map[string]string{secretClientID: clientID, secretTenantID: tenantID} {
		if value == "" {
			return nil, &platform.CredentialError{Msg: fmt.Sprintf(
				"secret %s/%s has no %q; the cloud credential operator writes one in every mode, so this secret was not written by it",
				secret.Namespace, secret.Name, name)}
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

	return nil, &platform.CredentialError{Msg: fmt.Sprintf(
		"secret %s/%s carries neither %q nor %q, so there is no way to authenticate with it",
		secret.Namespace, secret.Name, secretClientSecret, secretFederatedTokenFile)}
}

// reconcileCredentialsRequest brings the request to what we want it to
// be, rather than creating it once. An administrator narrowing the
// permissions, or this operator widening them in a later release, would
// otherwise leave the cluster serving a request nobody wrote. It also
// adopts a request that has no owner, which is what a release made
// before this one left behind.
func reconcileCredentialsRequest(ctx context.Context, c client.Client, namespace string, owner metav1.OwnerReference) error {
	desired := desiredCredentialsRequest(namespace, owner)

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

	adopted := ensureOwnerReference(existing, owner)
	if reflect.DeepEqual(existing.Object["spec"], desired.Object["spec"]) && !adopted {
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

func desiredCredentialsRequest(namespace string, owner metav1.OwnerReference) *unstructured.Unstructured {
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
	cr.SetOwnerReferences([]metav1.OwnerReference{owner})
	return cr
}

// ensureOwnerReference names the configuration among the object's
// owners and reports whether that changed anything. A request made
// before this operator set an owner has none, and a configuration that
// was deleted and recreated leaves one whose UID no longer resolves; in
// both cases the object would outlive the configuration it belongs to.
func ensureOwnerReference(obj *unstructured.Unstructured, owner metav1.OwnerReference) bool {
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if refs[i].Kind != owner.Kind || refs[i].Name != owner.Name {
			continue
		}
		if refs[i].UID == owner.UID {
			return false
		}
		refs[i] = owner
		obj.SetOwnerReferences(refs)
		return true
	}
	obj.SetOwnerReferences(append(refs, owner))
	return true
}
