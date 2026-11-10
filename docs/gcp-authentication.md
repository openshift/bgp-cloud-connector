# GCP authentication

The operator needs GCP credentials to read the Cloud Router, rewrite its BGP peers, reconcile the Network Connectivity Center (NCC) router appliance spoke, and set `canIpForward` and nested virtualisation on the router nodes (see [cloud-integration.md](cloud-integration.md)). It gets them the same way it does on [AWS](aws-authentication.md) and [Azure](azure-authentication.md): it creates a `CredentialsRequest` for itself and reads the secret the **Cloud Credential Operator (CCO)** writes in reply. What you have to set up depends on how your cluster hands out GCP credentials.

## Which setup applies

| Your cluster | What you do | Details |
|:---|:---|:---|
| **GCP IPI, mint** (the default) | Nothing | [Mint](#mint) |
| **GCP IPI, Manual mode with workload identity federation** (`ccoctl`) | Create a GCP service account for the operator, let the operator's ServiceAccount impersonate it, and give its details to the Subscription | [Workload identity federation](#workload-identity-federation) |

## How the operator gets a credential

During reconciliation `ResolveCredentials` (`internal/platform/gcp/credentials.go`) reads the secret `bgp-cloud-connector-gcp-credentials` in the operator's namespace. CCO writes it in reply to the operator's `CredentialsRequest`, `bgp-cloud-connector-gcp` in `openshift-cloud-credential-operator`, which asks for these permissions:

```
compute.routers.get
compute.routers.update
compute.networks.updatePolicy
compute.networks.get
compute.instances.get
compute.instances.update
compute.zoneOperations.get
compute.regionOperations.get
networkconnectivity.operations.get
networkconnectivity.hubs.get
networkconnectivity.spokes.get
networkconnectivity.spokes.list
networkconnectivity.spokes.create
networkconnectivity.spokes.update
networkconnectivity.spokes.delete
```

The secret holds `service_account.json`, and its `type` says which kind of credential it is: `service_account` is a key for a service account CCO made, `external_account` is a federated configuration that exchanges the operator's projected ServiceAccount token, at `/var/run/secrets/openshift/serviceaccount/token` with audience `openshift`, for a token for a service account you made. The operator passes the type to the Google client explicitly rather than letting it infer one, and names `googleapis.com` as the universe, because otherwise the client asks the metadata server, which no pod here can reach.

The secret is read on every reconcile, so a rotated credential takes effect without a restart. The `CredentialsRequest` is reconciled on every pass too, so it keeps up with the permissions the running release asks for. It names the `BGPCloudConfiguration` as its owner, so deleting the configuration deletes the request and, on a cluster that mints, the service account CCO made for it.

Where there is no secret, the operator falls back to Application Default Credentials. That is what serves a manager run from your desk against a `gcloud auth application-default login`; in a pod nothing is found, because the metadata server is unreachable from the pod network, and the operator raises its request and waits.

## Mint

`credentialsMode` unset or `Mint`, which is how GCP IPI installs by default. **Nothing to set up.** CCO creates a service account granted the permissions above, and puts a key for it in the secret. The `BGPCloudConfiguration` reports `CloudEndpointsDiscovered=False` with reason `WaitingForCloudCredentials` for the few seconds this takes, then proceeds.

## Workload identity federation

`credentialsMode: Manual` with a service account issuer, which is what `ccoctl gcp create-all` installs. There is no long-lived key in the cluster: each component presents its projected ServiceAccount token to a workload identity pool and impersonates a GCP service account. CCO cannot mint anything here. It writes the operator's secret only when the `CredentialsRequest` names a service account to impersonate and the pool provider to exchange against, and only you can create that service account: the operator has no credentials with which to create one for itself.

You need the project id, the project number, and the pool and provider the cluster was installed with. `ccoctl gcp create-all --name=<name>` names both the pool and the provider `<name>`.

```bash
project=<project id>
project_number=$(gcloud projects describe "$project" --format='value(projectNumber)')
pool=<pool id>
provider=<provider id>
```

**Step 1: create the service account and grant it the permissions above.**

```bash
gcloud iam service-accounts create bgp-cloud-connector --project "$project" \
    --display-name bgp-cloud-connector
email=bgp-cloud-connector@$project.iam.gserviceaccount.com

gcloud iam roles create bgpCloudConnector --project "$project" --title bgp-cloud-connector \
    --permissions compute.routers.get,compute.routers.update,compute.networks.updatePolicy,compute.networks.get,compute.instances.get,compute.instances.update,compute.zoneOperations.get,compute.regionOperations.get,networkconnectivity.operations.get,networkconnectivity.hubs.get,networkconnectivity.spokes.get,networkconnectivity.spokes.list,networkconnectivity.spokes.create,networkconnectivity.spokes.update,networkconnectivity.spokes.delete
gcloud projects add-iam-policy-binding "$project" --member "serviceAccount:$email" \
    --role "projects/$project/roles/bgpCloudConnector"
```

**Step 2: let the operator's ServiceAccount impersonate it** through the pool. This is the binding `ccoctl` makes for the cluster's own components:

```bash
subject=system:serviceaccount:openshift-bgp-cloud-connector:openshift-bgp-cloud-connector-controller-manager
gcloud iam service-accounts add-iam-policy-binding "$email" --project "$project" \
    --role roles/iam.workloadIdentityUser \
    --member "principal://iam.googleapis.com/projects/$project_number/locations/global/workloadIdentityPools/$pool/subject/$subject"
```

The subject names the namespace the operator runs in; change it if you installed somewhere other than `openshift-bgp-cloud-connector`.

**Step 3: give the operator the identity.**

- **Installing from OperatorHub:** the console asks for the GCP Project Number, Pool ID, Provider ID and Service Account Email, because the CSV declares `features.operators.openshift.io/token-auth-gcp: "true"`, and sets them on the Subscription for you.
- **Installing by hand:** put them in the Subscription:

  ```yaml
  spec:
    config:
      env:
      - name: PROJECT_NUMBER
        value: "<project number>"
      - name: POOL_ID
        value: <pool id>
      - name: PROVIDER_ID
        value: <provider id>
      - name: SERVICE_ACCOUNT_EMAIL
        value: <service account email>
  ```

The operator then adds `serviceAccountEmail` and `audience` (`//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`) to its `CredentialsRequest`, with `cloudTokenPath`, and CCO writes a secret holding the federated configuration.

All four variables are needed. With only some of them, with a project id where the number belongs, or with a slash in the pool or provider, the operator sends nothing to CCO. Until CCO has written the secret, the operator reports the problem on the `BGPCloudConfiguration`, naming the variable. Once the secret exists, it keeps using it, leaves the `CredentialsRequest` as it is, and only writes the problem to its log.

The variables are read from the operator's environment, so changing them needs a new pod. Setting them through the Subscription changes the Deployment and rolls the pod; the operator updates its existing `CredentialsRequest` on the next pass.

Use Manual approval for the Subscription, as the console suggests on these clusters. A later release may need permissions the service account does not yet hold, and Manual approval gives you the chance to grant them before upgrading.

## Troubleshooting

The operator reports credential state through the `CloudEndpointsDiscovered` condition on `BGPCloudConfiguration`:

| Reason | Meaning | What to do |
|:---|:---|:---|
| `WaitingForCloudCredentials` | The operator has asked CCO for credentials and the secret has not been written. It requeues every 10 seconds. | On a workload identity federation cluster, CCO writes nothing for a request that names no service account: check the four Subscription variables are set. If they are, check the `CredentialsRequest`'s status in `openshift-cloud-credential-operator`. |
| `CloudCredentialsInvalid` | A credential was found and the Google client could not be built from it. | Check the secret CCO wrote: `service_account.json` must be a JSON credential with a `type`. |
| `CloudDiscoveryFailed` | Either the operator could not build its request, or the credential works and a Compute or NCC call failed. | "failed to build the cloud platform" followed by a variable name means the Subscription's workload identity is incomplete or malformed. A `PERMISSION_DENIED` or 403 names the permission the service account lacks. On a workload identity federation cluster, if the error comes from exchanging the token, check that the `workloadIdentityUser` binding names this pool, namespace and ServiceAccount. |

Inspect the live conditions:

```bash
oc get bgpcloudconfiguration cluster -o jsonpath='{.status.conditions}' | jq .
```
