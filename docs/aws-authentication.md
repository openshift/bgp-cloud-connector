# AWS authentication

The operator needs AWS credentials to discover Route Server infrastructure and reconcile cloud-side resources (see [cloud-integration.md](cloud-integration.md)). It obtains them in one of two ways and works out which at runtime — there is no platform detection. This page describes the **recommended** setup and an **alternative** for the cases where it does not fit.

## How the operator chooses

During reconciliation the operator asks the AWS SDK whether the pod already has usable credentials: `BGPCloudConfigurationReconciler.Reconcile` builds the AWS platform (`buildAWSPlatform`), which builds the default credential chain and forces a `Retrieve` (`ResolveCredentials` → `ambientCredentials` in `internal/platform/aws/credentials.go`). Loading the chain is not enough — it is only truly resolved by retrieving from it.

- **Nothing to retrieve →** the operator creates a `CredentialsRequest` and lets the **Cloud Credential Operator (CCO)** provide credentials. This is the **[recommended path](#recommended-cco-managed-credentials)**.
- **Credentials already present →** the operator uses them and asks the cluster for nothing. The default chain resolves any ambient credentials in the pod — environment variables, a mounted shared-credentials file, or an injected web-identity token. The setup this guide covers for producing them in a deployed operator is the **[alternative IRSA path](#alternative-pod-injected-irsa-credentials)** (an annotated ServiceAccount, so the pod identity webhook injects a token); the other sources are equivalent to the SDK but are not managed setups documented here.

You do not choose a path directly — you perform one of the setups below, and the runtime probe lands on the matching one.

**CCO = Cloud Credential Operator**, the standard OpenShift operator (namespace `openshift-cloud-credential-operator`) that provisions cloud credentials from a `CredentialsRequest`. Its `credentialsMode` (Mint / Passthrough / Manual) determines what it can do. The operator is packaged for the CCO flow: its CSV declares `features.operators.openshift.io/token-auth-aws: "true"`, so an OperatorHub install prompts for the role ARN and wires up CCO for you.

## Which setup applies

| Your cluster | Recommended setup | Details |
|:---|:---|:---|
| **IPI, mint mode** (`credentialsMode` unset or `Mint`) | **Nothing** — CCO mints an IAM user automatically | [Recommended → Mint mode](#mint-mode-ipi) |
| **Manual / STS via `ccoctl`, ROSA, OSD, OCP Core** (OIDC configured) | Set `ROLEARN` (the OperatorHub console sets it for you); the IAM role must pre-exist | [Recommended → Manual/STS mode](#manual--sts-mode) |
| CCO unavailable/disabled, already standardized on IRSA, or quick dev/test | Annotate the operator's ServiceAccount (IRSA) | [Alternative](#alternative-pod-injected-irsa-credentials) |

---

## Recommended: CCO-managed credentials

The operator creates a `CredentialsRequest` named `bgp-cloud-connector-aws` carrying exactly the permissions it needs, and reads the `credentials` key of the secret CCO writes into the operator's namespace. That key is a shared-credentials ini file, and CCO writes one whatever mode the cluster is in — which is why this single path serves both sub-cases below. The credential type differs by mode: **Mint** and **passthrough** store a long-lived IAM user access-key pair, static until rotated or revoked; **Manual/STS** stores no credentials at all — only the role ARN and the path to a projected web-identity token (written into the ini as `web_identity_token_file`), which the AWS SDK exchanges for temporary STS credentials and refreshes automatically. (The **IRSA** alternative below is not served by this secret at all — the pod-identity webhook injects AWS environment variables that reference the ServiceAccount token directly.) `ROLEARN` is read from the operator's process environment (`os.Getenv`), so adding or changing it requires the operator pod to be recreated — a running pod does not observe a new value on its own. Setting it through the Subscription changes the Deployment and rolls the pod automatically; once the new pod starts, the operator reconciles its own `CredentialsRequest` with the updated role on the next pass.

### Mint mode (IPI)

`credentialsMode` unset or `Mint`. **Nothing to set up.** CCO creates an IAM user and puts its key pair in the secret. The `BGPCloudConfiguration` reports `CloudEndpointsDiscovered=False` with reason `WaitingForCloudCredentials` for the few seconds this takes, then proceeds.

### Manual / STS mode

`credentialsMode: Manual` with an OIDC provider — what `ccoctl` installs, and what ROSA uses. CCO cannot mint anything here, so you give the operator an IAM role ARN and CCO federates a short-lived token against it.

**Step 1 — Create the IAM role** (see [Create the IAM role and policy](#create-the-iam-role-and-policy) below). The role must exist first; only you can create it, because the operator has no credentials with which to create a role for itself.

**Step 2 — Give the operator the role ARN via `ROLEARN`:**

- **Installing from OperatorHub:** the console prompts for the role ARN and sets `ROLEARN` for you (driven by the CSV's `features.operators.openshift.io/token-auth-aws` annotation).
- **Installing by hand:** put it in the Subscription:

  ```yaml
  spec:
    config:
      env:
      - name: ROLEARN
        value: arn:aws:iam::<account>:role/<role>
  ```

The operator then adds `stsIAMRoleARN` and `cloudTokenPath` to its `CredentialsRequest` (both together — CCO reads a role ARN without a token path as a request it cannot serve), and CCO writes a secret naming that role and the token the operator projects at `/var/run/secrets/openshift/serviceaccount/token`.

---

## Alternative: pod-injected IRSA credentials

Instead of CCO, you can annotate the operator's ServiceAccount with an IAM role ARN; the pod identity webhook then injects a web-identity token at pod creation and the SDK's default credential chain resolves it. Use this when:

- **CCO is unavailable or disabled** on the cluster (removed, or in a mode that will not provision the request).
- The cluster is **already standardized on IRSA / pod identity** and you prefer not to involve CCO.
- You want a **quick dev/test setup** without going through a Subscription or OperatorHub `ROLEARN` flow.

**Step 1 — Create the IAM role** (see [Create the IAM role and policy](#create-the-iam-role-and-policy) below).

**Step 2 — Annotate the operator's ServiceAccount:**

```bash
oc annotate serviceaccount openshift-bgp-cloud-connector-controller-manager \
  -n openshift-bgp-cloud-connector \
  eks.amazonaws.com/role-arn=arn:aws:iam::${AWS_ACCOUNT_ID}:role/bgp-cloud-connector
```

The OIDC webhook automatically injects `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` environment variables into the operator pod. The AWS SDK's default credential chain picks these up — no explicit credential configuration is needed in the CR.

> **Limitation — restart required only when the injected values change.** The OIDC webhook injects `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` at **pod creation time** only. Restart the operator when those injected values change: you complete the IRSA setup while it is already running, or you change the ServiceAccount annotation (which changes the injected role ARN or token path).
>
> ```bash
> oc rollout restart deployment/openshift-bgp-cloud-connector-controller-manager -n openshift-bgp-cloud-connector
> ```
>
> Correcting the IAM role's trust policy or permissions on the AWS side, with the injected role ARN and token path unchanged, does **not** need a restart — the next reconcile calls STS again and picks up the corrected policy. The recommended CCO path has no injection step at all — it re-reads its own `CredentialsRequest` on each reconcile.

---

## Create the IAM role and policy

Both the recommended Manual/STS path and the IRSA alternative need the same IAM role, with the same trust-policy shape. Create it once.

**Create the role with a trust policy for the operator's ServiceAccount:**

```bash
# Get the cluster's OIDC provider (works on any OCP cluster: ROSA, OSD, OCP Core)
OIDC_PROVIDER=$(oc get authentication cluster -o jsonpath='{.spec.serviceAccountIssuer}' | sed 's|https://||')
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

aws iam create-role --role-name bgp-cloud-connector \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": {"Federated": "arn:aws:iam::'$AWS_ACCOUNT_ID':oidc-provider/'$OIDC_PROVIDER'"},
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "'$OIDC_PROVIDER':sub": "system:serviceaccount:openshift-bgp-cloud-connector:openshift-bgp-cloud-connector-controller-manager",
          "'$OIDC_PROVIDER':aud": ["openshift", "sts.amazonaws.com"]
        }
      }
    }]
  }'
```

> The `aud` condition lists both audiences because the two paths present different ones: the Manual/STS token the operator projects for CCO carries `openshift` (the audience `ccoctl` registers with the cluster's OIDC issuer), while the IRSA webhook injects a token with `sts.amazonaws.com`. Listing both lets the single shared role serve either path; do not narrow it to one value.
>
> `serviceAccountIssuer` is the cluster's own OIDC endpoint, read from its config. It returns the same value ROSA's `rosa describe cluster -c <name> -o json | jq -r '.aws.sts.oidc_endpoint_url'` does, without depending on the `rosa` CLI, so this command works on ROSA, OSD, and OCP Core alike.

**Attach the required permissions policy:**

```bash
aws iam put-role-policy --role-name bgp-cloud-connector \
  --policy-name bgp-cloud-connector-policy \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "sts:GetCallerIdentity",
          "ec2:DescribeRouteServers",
          "ec2:DescribeRouteServerEndpoints",
          "ec2:DescribeSubnets",
          "ec2:DescribeRouteServerPeers",
          "ec2:CreateRouteServerPeer",
          "ec2:DeleteRouteServerPeer",
          "ec2:CreateTags",
          "ec2:DescribeInstances",
          "ec2:ModifyNetworkInterfaceAttribute"
        ],
        "Resource": "*"
      }
    ]
  }'
```

> **Least privilege.** This example grants every action on `Resource: "*"` for simplicity. The read-only and STS actions (`sts:GetCallerIdentity`, the `ec2:Describe*` calls) do not support resource-level restrictions and must stay `*`. For a hardened deployment, scope the **mutating** actions instead of leaving them on `*`:
> - Restrict `ec2:CreateRouteServerPeer` / `ec2:DeleteRouteServerPeer` to your Route Server and peer ARNs.
> - Gate `ec2:CreateTags` (and the peer actions) with an `aws:RequestTag`/`aws:ResourceTag` condition on the operator's `managed-by` tag, so it can only tag/modify resources it owns.
> - Scope `ec2:ModifyNetworkInterfaceAttribute` to your cluster's ENIs — by ENI ARN, or with an `aws:ResourceTag` ownership condition.
>
> Verify resource-level and condition-key support for each action against the current [EC2 IAM reference](https://docs.aws.amazon.com/service-authorization/latest/reference/list_amazonec2.html) before applying — Route Server actions are relatively new and their supported ARN formats and condition keys change.

Complete this **before** creating the `BGPCloudConfiguration` CR with `spec.aws`.

---

## Troubleshooting

The operator reports credential state through the `CloudEndpointsDiscovered` condition on `BGPCloudConfiguration`:

| Reason | Meaning | What to do |
|:---|:---|:---|
| `CloudCredentialsInvalid` | Credentials were found but `sts:GetCallerIdentity` failed. | Correct the credentials, the IAM role trust policy, its permissions, or the secret CCO wrote, then trigger reconciliation again. This is a terminal condition — it does not requeue or recover on its own. |
| `WaitingForCloudCredentials` | The cluster has been asked for credentials and has not yet provided them. Not `Degraded` — the CR stays `Configuring` and requeues every 10s. | If it never clears on a Manual/STS cluster, `ROLEARN` is unset — CCO ignores a request without `stsIAMRoleARN`. |

If you used the IRSA alternative and changed the ServiceAccount annotation (the injected role ARN or token path) while the operator was running, restart it (see the [restart limitation](#alternative-pod-injected-irsa-credentials)) — the webhook injects those values only at pod creation. Correcting the IAM role's trust policy or permissions with the injected values unchanged does not need a restart; the next reconcile picks it up.

Inspect the live conditions:

```bash
oc get bgpcloudconfiguration cluster -o jsonpath='{.status.conditions}' | jq .
```
