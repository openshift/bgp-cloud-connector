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
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// CredentialsRequestGVK is the cloud credential operator's API. It is
// addressed as unstructured for the same reason as everything else
// downstream of this operator: the typed package drags in a dependency
// tree out of all proportion to one object.
var CredentialsRequestGVK = schema.GroupVersionKind{
	Group: "cloudcredential.openshift.io", Version: "v1", Kind: "CredentialsRequest",
}

const (
	// CredentialsRequestName is the request the operator makes for itself.
	CredentialsRequestName = "bgp-cloud-connector-aws"

	// CredentialsRequestNamespace is where the cloud credential operator
	// looks for requests. It is not where the secret lands.
	CredentialsRequestNamespace = "openshift-cloud-credential-operator"

	// CredentialsSecretName is the secret CCO writes, in the operator's
	// own namespace.
	CredentialsSecretName = "bgp-cloud-connector-aws-credentials"

	// ServiceAccountName is the operator's ServiceAccount. On an STS
	// cluster it is the subject the IAM role's trust policy names.
	ServiceAccountName = "openshift-bgp-cloud-connector-controller-manager"

	// credentialsKey is the one part of the secret worth reading. CCO
	// writes it in every mode it operates in -- an ini file holding
	// either a key pair or a role and a token path -- and repairs
	// secrets that lack it. Reading it rather than the individual keys
	// is what lets one code path serve a cluster that mints and a
	// cluster that federates.
	credentialsKey = "credentials"

	// credentialsFileName is where that ini is put for the SDK to read.
	// The name is fixed rather than random because this runs on every
	// reconcile, and a fresh temporary file each time would be a leak
	// with a five minute period.
	credentialsFileName = "bgp-cloud-connector-aws-credentials.ini"

	// roleARNEnvVar carries the IAM role the operator should assume on
	// an STS cluster. OLM sets it from the Subscription, which is what
	// the console writes when the CSV declares token-auth-aws.
	roleARNEnvVar = "ROLEARN"

	// cloudTokenPath is where the deployment projects the bound
	// ServiceAccount token, and what CCO writes into the secret as
	// web_identity_token_file.
	cloudTokenPath = "/var/run/secrets/openshift/serviceaccount/token"

	// defaultProfile is the only section CCO writes.
	defaultProfile = "default"
)

// CredentialOptions are the config.LoadDefaultConfig options that supply
// the operator's AWS credentials. ResolveCredentials never returns an
// empty slice and a nil error together, so an empty one reaching the SDK
// means resolution was skipped -- which is the silent failure this file
// exists to prevent.
type CredentialOptions []func(*config.LoadOptions) error

// policyActions is what the operator needs to do its job, and no more:
// discovery of the route server estate, the peers it manages, the tags
// that mark them as its own, and source/destination checking on the
// router nodes' interfaces.
var policyActions = []string{
	"ec2:DescribeRouteServers",
	"ec2:DescribeRouteServerEndpoints",
	"ec2:DescribeRouteServerPeers",
	"ec2:DescribeSubnets",
	"ec2:DescribeInstances",
	"ec2:CreateRouteServerPeer",
	"ec2:DeleteRouteServerPeer",
	"ec2:CreateTags",
	"ec2:ModifyNetworkInterfaceAttribute",
}

// ambientCredentials asks the AWS SDK whether the pod already has
// credentials. On ROSA it does: the pod identity webhook injects a web
// identity token and a role ARN when the ServiceAccount is annotated, and
// the default chain resolves them. Overridden in tests.
var ambientCredentials = func(ctx context.Context, region string) (awssdk.CredentialsProvider, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, err
	}
	// LoadDefaultConfig assembles a chain without consulting it, so the
	// question of whether anything is actually there is only answered by
	// retrieving.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, err
	}
	return cfg.Credentials, nil
}

// ResolveCredentials returns the options that give the operator AWS
// credentials, asking the cluster for them if the pod has none.
//
// Where the pod already has credentials -- ROSA, where the pod identity
// webhook injects them, or a manager run from a desk with a profile --
// they are used and the cluster is left alone. Otherwise the operator
// asks the cloud credential operator and reads what comes back.
//
// Deciding between the two by asking the SDK, rather than by inspecting
// the Infrastructure CR or sniffing environment variables, keeps the
// decision on the thing that actually matters: whether credentials can
// be retrieved.
//
// What comes back is an ini file rather than a pair of keys, and it is
// handed to the SDK as a shared credentials file rather than taken
// apart here. That is the whole reason one path serves both kinds of
// cluster: a minting cluster puts a key pair in it, an STS cluster puts
// a role and a token path, and the SDK already knows both.
func ResolveCredentials(ctx context.Context, c client.Client, namespace, region string) (CredentialOptions, error) {
	logger := log.FromContext(ctx)

	if provider, err := ambientCredentials(ctx, region); err == nil {
		logger.V(1).Info("using the credentials already available to the pod")
		return CredentialOptions{config.WithCredentialsProvider(provider)}, nil
	}

	if err := reconcileCredentialsRequest(ctx, c, namespace); err != nil {
		return nil, err
	}

	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: CredentialsSecretName, Namespace: namespace}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil, pendingError(namespace)
	case err != nil:
		return nil, fmt.Errorf("reading secret %s/%s: %w", namespace, CredentialsSecretName, err)
	}

	ini := secret.Data[credentialsKey]
	if len(ini) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no %q key; the cloud credential operator writes one in every mode, so this secret was not written by it",
			namespace, CredentialsSecretName, credentialsKey)
	}

	path, err := writeCredentialsFile(ini)
	if err != nil {
		return nil, err
	}

	logger.V(1).Info("using credentials provided by the cloud credential operator", "secret", CredentialsSecretName)
	// The profile is pinned because CCO always writes [default] and the
	// SDK would otherwise honour AWS_PROFILE and look for a section this
	// file will never have.
	return CredentialOptions{
		config.WithSharedConfigProfile(defaultProfile),
		config.WithSharedCredentialsFiles([]string{path}),
	}, nil
}

// pendingError says what is being waited for, and on a cluster that
// federates, why the wait will not end on its own.
//
// The cloud credential operator ignores a request carrying no
// stsIAMRoleARN and marks it provisioned anyway, so the CredentialsRequest
// reports success while no secret is ever written. This condition is the
// only place the cause is stated, which is why it names the environment
// variable rather than describing the symptom.
func pendingError(namespace string) error {
	if roleARN := os.Getenv(roleARNEnvVar); roleARN != "" {
		return fmt.Errorf("%w: secret %s/%s has not been written yet, for role %s",
			platform.ErrCredentialsPending, namespace, CredentialsSecretName, roleARN)
	}
	return fmt.Errorf("%w: secret %s/%s has not been written. On a cluster with credentialsMode Manual the cloud credential operator will not write it and reports the request provisioned regardless; set %s in the Subscription to the IAM role ARN the operator should assume",
		platform.ErrCredentialsPending, namespace, CredentialsSecretName, roleARNEnvVar)
}

// writeCredentialsFile puts the ini somewhere the SDK can read it. The
// SDK takes a path and offers no way to pass the bytes, so there has to
// be a file; the rename makes it appear whole rather than half written.
func writeCredentialsFile(ini []byte) (_ string, err error) {
	dir := os.TempDir()
	path := filepath.Join(dir, credentialsFileName)

	tmp, err := os.CreateTemp(dir, credentialsFileName+".*")
	if err != nil {
		return "", fmt.Errorf("creating the credentials file: %w", err)
	}

	// The rename consumes the temporary, so every path that leaves here
	// without renaming has one to clear up, and there is exactly one of
	// those: this. Whether the removal worked is joined to the error
	// rather than dropped -- a file holding AWS credentials, left behind
	// in a world-readable directory, is worth saying out loud, and
	// joining it reports that without displacing the reason we failed.
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(tmp.Name()))
		}
	}()

	// Closed here, on the way out, because the close after a successful
	// write is where a delayed write error surfaces and so has to be
	// reported on its own. These two already have their reason and only
	// need the descriptor released, but a close that fails still says
	// the bytes may not be where we think.
	if err := tmp.Chmod(0o600); err != nil {
		return "", errors.Join(fmt.Errorf("setting permissions on the credentials file: %w", err), tmp.Close())
	}
	if _, err := tmp.Write(ini); err != nil {
		return "", errors.Join(fmt.Errorf("writing the credentials file: %w", err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing the credentials file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("installing the credentials file: %w", err)
	}
	return path, nil
}

// reconcileCredentialsRequest creates the request and keeps its spec at
// what we want it to be. It has to update rather than only create: the
// role ARN arrives by editing the Subscription, which happens after the
// operator has already made its request, and a request created without
// one would leave the operator waiting on a secret CCO has no reason to
// write. Only the spec is written, and only when it differs, so CCO's
// status and an administrator's other edits survive.
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
		log.FromContext(ctx).Info("asked the cloud credential operator for AWS credentials",
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
	log.FromContext(ctx).Info("updated the request for AWS credentials",
		"credentialsRequest", CredentialsRequestName)
	return nil
}

func desiredCredentialsRequest(namespace string) *unstructured.Unstructured {
	actions := make([]interface{}, 0, len(policyActions))
	for _, action := range policyActions {
		actions = append(actions, action)
	}

	providerSpec := map[string]interface{}{
		"apiVersion": "cloudcredential.openshift.io/v1",
		"kind":       "AWSProviderSpec",
		"statementEntries": []interface{}{
			map[string]interface{}{
				"effect":   "Allow",
				"action":   actions,
				"resource": "*",
			},
		},
	}

	spec := map[string]interface{}{
		"secretRef": map[string]interface{}{
			"name":      CredentialsSecretName,
			"namespace": namespace,
		},
		"serviceAccountNames": []interface{}{ServiceAccountName},
		"providerSpec":        providerSpec,
	}

	// Both fields together or neither. CCO reads their absence as a
	// request to mint an IAM user, and reads a role ARN without a token
	// path as a request it cannot serve.
	if roleARN := os.Getenv(roleARNEnvVar); roleARN != "" {
		providerSpec["stsIAMRoleARN"] = roleARN
		spec["cloudTokenPath"] = cloudTokenPath
	}

	cr := &unstructured.Unstructured{Object: map[string]interface{}{"spec": spec}}
	cr.SetGroupVersionKind(CredentialsRequestGVK)
	cr.SetName(CredentialsRequestName)
	cr.SetNamespace(CredentialsRequestNamespace)
	return cr
}
