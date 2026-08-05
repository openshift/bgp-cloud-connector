# CUDN BGP Routing Operator

Kubernetes operator for OpenShift that automates L3 direct routing between CUDN Pod networks and external networks via BGP. Replaces the manual in-cluster steps from the [rosa-bgp PoC](https://github.com/msemanrh/rosa-bgp).

The operator is **cloud platform aware**. When platform configuration is provided (e.g. AWS), the operator auto-discovers cloud BGP infrastructure (Route Server endpoints, neighbor IPs, remote ASN, AZ mapping) and manages cloud-side networking resources (Route Server peers, SourceDestCheck) to keep BGP peering and traffic forwarding current for node changes.

## Table of Contents

- [Architecture](#architecture)
- [Cloud Platform Integration](#cloud-platform-integration)
- [Custom Resource Definitions](#custom-resource-definitions)
- [Controller Reconciliation](#controller-reconciliation)
- [Development](#development)
- [Automated Testing](#automated-testing)
- [KubeVirt VM Testing](#kubevirt-vm-testing)

---

## Architecture

The overall solution (e.g. AWS) has two layers. The operator manages the in-cluster layer and, when platform integration is configured, also discovers cloud BGP infrastructure and reconciles cloud-side networking resources.

```
┌──────────────────────────────────────────────────────────────────────┐
│  Cloud Infrastructure (Terraform provisions once)                    │
│                                                                      │
│  VPC / subnets / route server / TGW / etc.                           │
│  Machine pools labeled bgp_router: "true" (Terraform input)          │
│  Terraform outputs: Route Server IDs, local BGP ASN, AWS region      │
└──────────────────────────────────┬───────────────────────────────────┘
                                   │ user copies terraform outputs
                                   │ (RS IDs, ASN) into CR spec
                                   ▼
┌──────────────────────────────────────────────────────────────────────┐
│  In-Cluster (Operator)                                               │
│                                                                      │
│  CUDNBgpConfig CR (singleton — BGP infra)                            │
│  ├── Patch Network.operator.openshift.io (enable FRR)                │
│  ├── [if platform configured] Discover RS endpoints, neighbor IPs,   │
│  │   remote ASN, and AZ mapping via cloud API                        │
│  ├── FRRConfiguration per AZ (BGP sessions to local RS endpoints)    │
│  └── [if platform configured] Reconcile cloud networking on          │
│       node changes (Route Server peers, SourceDestCheck, etc.)       │
│                                                                      │
│  CUDNBgpRouting CR (one per application project)                     │
│  ├── ClusterUserDefinedNetwork (targets user-labeled namespaces)     │
│  └── Shared RouteAdvertisements (all CUDNs with advertise=true)      │
└──────────────────────────────────────────────────────────────────────┘
```

---

## Cloud Platform Integration

BGP peering between OCP worker nodes and a cloud BGP service (e.g. AWS VPC Route Server) requires configuration on **both sides**:

- **OCP side:** BGP-enabled worker nodes *initiate* BGP sessions toward the cloud BGP service endpoints.
- **Cloud side:** The cloud BGP service must be told to *accept* sessions from each node's IP and ASN.

When a BGP-enabled worker node is replaced (upgrade, spot termination, scaling), two things break at the cloud layer:

1. **Stale BGP peers** — the cloud BGP service still has a peer registered for the old node's IP. The new node cannot establish a session because it has no peer registration.
2. **Forwarding rules revert** — the new node's network interface defaults to dropping forwarded traffic (e.g. AWS `SourceDestCheck=true`), silently breaking all CUDN pod traffic through that node.

Without platform integration, these require manual intervention (e.g. re-running `terraform apply`). With platform integration, the operator creates and reconciles the cloud-side BGP peering and traffic forwarding for node changes.

Kubernetes has out-of-tree cloud controller managers (e.g. [cloud-provider-aws](https://github.com/kubernetes/cloud-provider-aws)) that implement a broad `cloudprovider.Interface`. This operator uses platform API (e.g. `aws-sdk-go-v2`) directly instead because it only needs a narrow slice of platform functionality (Route Server peers + SourceDestCheck). Importing a full cloud controller manager would bring a large dependency graph for minimal benefit. The architectural pattern (interface-based, per-provider package) is the same. The operator authenticates to AWS via IRSA — the ROSA HCP cluster's pre-configured OIDC provider allows the operator's ServiceAccount to assume an IAM role with short-lived, automatically rotated credentials.

### AWS Platform

When AWS platform integration is configured, the operator performs additional actions during `CUDNBgpConfig` reconciliation:

| Action | AWS API calls | Trigger |
|:---|:---|:---|
| Verify credentials | `sts:GetCallerIdentity` | Every reconcile (before any EC2 calls); credentials obtained via IRSA |
| Discover Route Server infrastructure | `DescribeRouteServers`, `DescribeRouteServerEndpoints`, `DescribeSubnets` | Every reconcile (before FRR configuration) |
| Reconcile Route Server peers | `DescribeRouteServerPeers`, `CreateRouteServerPeer`, `DeleteRouteServerPeer`, `CreateTags` | BGP-enabled worker node added, removed, or IP changed |
| Disable SourceDestCheck | `DescribeInstances`, `ModifyNetworkInterfaceAttribute` | New BGP-enabled worker node detected |

**Auto-discovery:** The operator discovers Route Server endpoints, their ENI addresses (BGP neighbor IPs), availability zones, and the Route Server's remote ASN automatically from the provided Route Server IDs. The user does not need to specify per-AZ endpoint IDs or neighbor addresses — the operator derives them via `DescribeRouteServerEndpoints` (endpoint ID + ENI address + subnet), `DescribeSubnets` (subnet → AZ mapping), and `DescribeRouteServers` (remote ASN). Discovered data is written to `status.aws` for observability. This also drives FRR configuration generation — the operator creates one FRRConfiguration per discovered AZ with the discovered neighbor addresses.

Route Server peers are created **per-AZ** — each BGP-enabled worker node is peered with its local AZ's Route Server endpoints. Peers are tagged with `managed-by: cudn-bgp-routing-operator/<infrastructureName>` for lifecycle management, where `<infrastructureName>` is read automatically from the OpenShift `Infrastructure/cluster` object (`status.infrastructureName`). This cluster-scoped tag ensures multiple clusters sharing the same VPC Route Server do not interfere with each other's peers. If a peer already exists at a desired IP but was not created by the operator (e.g. created manually or by Terraform), the operator adopts it by adding the `managed-by` tag rather than attempting to create a duplicate.

#### AWS authentication (IRSA)

The operator authenticates to AWS using IAM Roles for Service Accounts (IRSA). ROSA HCP clusters have an OIDC provider pre-configured, so the operator's ServiceAccount can assume an IAM role directly — no static credentials or Secrets are needed.

**The cluster admin must complete the following steps before creating the `CUDNBgpConfig` CR with `spec.aws`:**

**Step 1 — Create an IAM role with a trust policy for the operator's ServiceAccount:**

```bash
# Get the cluster's OIDC provider details
OIDC_PROVIDER=$(rosa describe cluster -c <cluster-name> -o json | jq -r '.aws.sts.oidc_endpoint_url' | sed 's|https://||')
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

aws iam create-role --role-name cudn-bgp-operator \
  --assume-role-policy-document '{
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Principal": {"Federated": "arn:aws:iam::'$AWS_ACCOUNT_ID':oidc-provider/'$OIDC_PROVIDER'"},
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "'$OIDC_PROVIDER':sub": "system:serviceaccount:openshift-cudn-bgp-routing:openshift-cudn-bgp-routing-controller-manager"
        }
      }
    }]
  }'
```

**Step 2 — Attach the required permissions policy:**

```bash
aws iam put-role-policy --role-name cudn-bgp-operator \
  --policy-name cudn-bgp-operator-policy \
  --policy-document '{
    "Version": "2012-10-17",
    "Statement": [
      {
        "Sid": "ReadOnly",
        "Effect": "Allow",
        "Action": [
          "sts:GetCallerIdentity",
          "ec2:DescribeRouteServers",
          "ec2:DescribeRouteServerEndpoints",
          "ec2:DescribeSubnets",
          "ec2:DescribeRouteServerPeers",
          "ec2:DescribeInstances"
        ],
        "Resource": "*"
      },
      {
        "Sid": "RouteServerPeerManagement",
        "Effect": "Allow",
        "Action": [
          "ec2:CreateRouteServerPeer",
          "ec2:DeleteRouteServerPeer",
          "ec2:CreateTags"
        ],
        "Resource": [
          "arn:aws:ec2:'$AWS_REGION':'$AWS_ACCOUNT_ID':route-server-endpoint/*",
          "arn:aws:ec2:'$AWS_REGION':'$AWS_ACCOUNT_ID':route-server-peer/*"
        ]
      },
      {
        "Sid": "DisableSourceDestCheck",
        "Effect": "Allow",
        "Action": "ec2:ModifyNetworkInterfaceAttribute",
        "Resource": "arn:aws:ec2:'$AWS_REGION':'$AWS_ACCOUNT_ID':network-interface/*"
      }
    ]
  }'
```

**Step 3 — Annotate the operator's ServiceAccount:**

```bash
oc annotate serviceaccount openshift-cudn-bgp-routing-controller-manager \
  -n openshift-cudn-bgp-routing \
  eks.amazonaws.com/role-arn=arn:aws:iam::${AWS_ACCOUNT_ID}:role/cudn-bgp-operator
```

The OIDC webhook automatically injects `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` environment variables into the operator pod. The AWS SDK's default credential chain picks these up — no explicit credential configuration is needed in the CR.

**Important:** The OIDC webhook only injects credentials at **pod creation time**. If the operator is already running when you complete the IRSA setup (or if you correct a misconfigured IAM role or ServiceAccount annotation), the running pod will not pick up the new credentials. You must restart the operator after making credential changes:

```bash
oc rollout restart deployment/openshift-cudn-bgp-routing-controller-manager -n openshift-cudn-bgp-routing
```

The operator reports credential issues as `AWSCredentialsInvalid` in the `CUDNBgpConfig` status. If you see this condition, verify the IAM role trust policy and permissions, then restart the operator.

### Multi-cloud extensibility

Currently only AWS platform integration is implemented but the design allows for additional providers. Each cloud maps to equivalent concepts:

| Concern | AWS | GCP (future) | Azure (future) |
|:---|:---|:---|:---|
| Endpoint discovery | DescribeRouteServerEndpoints + DescribeSubnets | Cloud Router interface listing | Azure Route Server IP config |
| BGP peering | VPC Route Server peers | Cloud Router peers | Azure Route Server peers |
| Forwarding fix | SourceDestCheck=false | canIpForward=true | IP forwarding=enabled |
| Identity | IRSA (via OIDC) | Workload Identity | Workload Identity |

### What the operator replaces

| Capability | PoC | Operator |
|:---|:---|:---|
| Enable FRR + routeAdvertisements | `oc patch` in shell script (step 6) | Controller patches Network CR on reconcile |
| Wait for FRR readiness | `sleep 60`, retry on error (step 6) | Controller polls FRR namespace and pods, requeues every 10s until ready |
| FRR BGP configuration | Single `FRRConfiguration` inline in script with all 6 neighbors hard-coded from `terraform output` — cross-AZ sessions fail (step 6) | One `FRRConfiguration` per AZ, neighbors and ASN auto-discovered from Route Server APIs |
| Namespace + CUDN | `oc apply -f yamls/` (step 7) | User creates labeled namespaces; controller creates ClusterUserDefinedNetwork per CUDNBgpRouting CR |
| RouteAdvertisements | `oc apply -f yamls/` (step 7) | Controller ensures a single shared RouteAdvertisements |
| Route Server peers | Terraform creates all peers statically (step 4) | Controller creates per-AZ peers dynamically, adopts/removes on node changes |
| Source/dest check | Terraform disables via shell script (step 4) | Controller disables on each BGP-enabled worker node's primary ENI |

### OLM / OperatorHub Packaging

| Field | Value |
|:---|:---|
| Package name | `cudn-bgp-routing-operator` |
| Default channel | `alpha` (moves to `stable` at GA) |
| Install modes | OwnNamespace, SingleNamespace |
| Target namespace | `openshift-cudn-bgp-routing` |
| Min OCP version | 4.21 (frr-k8s + CUDN + RouteAdvertisements) |
| Categories | Networking |
| Provider | Red Hat |

**Required APIs:** `FRRConfiguration` (frrk8s.metallb.io/v1beta1), `ClusterUserDefinedNetwork` (k8s.ovn.org/v1), `RouteAdvertisements` (k8s.ovn.org/v1)
**Owned APIs:** `CUDNBgpConfig`, `CUDNBgpRouting` (networking.openshift.io/v1alpha1)

> The operator constructs `ClusterUserDefinedNetwork`, `RouteAdvertisements`, and `FRRConfiguration` objects using `unstructured.Unstructured` rather than importing typed Go structs. The typed structs for CUDN and RouteAdvertisements live inside the monolithic `github.com/ovn-kubernetes/ovn-kubernetes/go-controller` module, which carries 100+ transitive dependencies (CNI plugins, netlink, kubevirt, the full `k8s.io/kubernetes` repo, etc.). Even `openshift/cluster-network-operator` avoids importing it for the same reason.

---

## Custom Resource Definitions

Two CRDs with clear separation of concerns:

- **`CUDNBgpConfig`** (singleton, cluster-scoped, **must be named `cluster`**) — shared BGP infrastructure. Owned by cluster admin.
- **`CUDNBgpRouting`** (one per CUDN, cluster-scoped) — declares a single CUDN network. Owned by application teams.

### CUDNBgpConfig (singleton - without cloud integration - using PoC configuration)

When `spec.aws` is absent, `spec.bgp.availabilityZones` is required — the user provides explicit neighbor addresses and per-AZ node selectors.

```yaml
apiVersion: networking.openshift.io/v1alpha1
kind: CUDNBgpConfig
metadata:
  name: cluster
spec:
  routerNodeSelector:
    bgp_router: "true"              # must match labels on BGP router machine pools

  bgp:
    localASN: 65001                  # terraform output rosa_bgp_asn
    livenessDetection: bgp-keepalive # bfd | bgp-keepalive (default)
    availabilityZones:
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1a
          bgp_router_subnet: "1"
        neighbors:
          - address: 10.0.1.47       # terraform output vpc1-rs1-subnet1-ep1_ip
            remoteASN: 64512         # terraform output vpc1-rs1-asn
          - address: 10.0.1.183      # terraform output vpc1-rs1-subnet1-ep2_ip
            remoteASN: 64512
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1b
          bgp_router_subnet: "2"
        neighbors:
          - address: 10.0.2.91       # terraform output vpc1-rs1-subnet2-ep1_ip
            remoteASN: 64512
          - address: 10.0.2.204      # terraform output vpc1-rs1-subnet2-ep2_ip
            remoteASN: 64512
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1c
          bgp_router_subnet: "3"
        neighbors:
          - address: 10.0.3.62       # terraform output vpc1-rs1-subnet3-ep1_ip
            remoteASN: 64512
          - address: 10.0.3.178      # terraform output vpc1-rs1-subnet3-ep2_ip
            remoteASN: 64512
```

### CUDNBgpConfig (singleton - with AWS integration - using PoC configuration)

When `spec.aws` is configured, `spec.bgp.availabilityZones` must not be set — the two sections are mutually exclusive (enforced by CRD validation). The operator auto-discovers Route Server endpoints, BGP neighbor addresses, remote ASN, and AZ mapping from the provided Route Server IDs.

```yaml
apiVersion: networking.openshift.io/v1alpha1
kind: CUDNBgpConfig
metadata:
  name: cluster
spec:
  routerNodeSelector:
    bgp_router: "true"              # must match labels on BGP router machine pools

  bgp:
    localASN: 65001                  # terraform output rosa_bgp_asn
    livenessDetection: bgp-keepalive # bfd | bgp-keepalive (default)

  aws:
    region: us-east-1                # terraform output aws_region
    routeServerIDs:                  # terraform output vpc1_route_server_ids
      - rs-0abc123456789abcd
```

### CUDNBgpRouting (one per application project - using PoC configuration)

```yaml
apiVersion: networking.openshift.io/v1alpha1
kind: CUDNBgpRouting
metadata:
  name: cudn1
spec:
  network:
    name: prod                       # CUDN selects namespaces with label cluster-udn=prod
    subnets:
      - 10.100.0.0/16
```

For dual-stack networks, specify both an IPv4 and an IPv6 subnet:

```yaml
apiVersion: networking.openshift.io/v1alpha1
kind: CUDNBgpRouting
metadata:
  name: cudn1
spec:
  network:
    name: prod
    subnets:
      - 10.100.0.0/16
      - 2001:db8::/64
```

> **IPv6 limitation (OCP 4.21):** OVN-Kubernetes requires an IPv6 underlay (i.e. IPv6-addressed nodes) to advertise IPv6 pod subnets via BGP. On IPv4-only clusters, dual-stack CUDNs can be created but IPv6 routes will not be advertised. This is an OVN-Kubernetes limitation, not this operator limitation.

### CRD field reference

**CUDNBgpConfig** (cluster admin creates once):

| Field | Required | Description |
|:---|:---|:---|
| `spec.routerNodeSelector` | Yes | Cluster-wide label selector for all BGP-enabled worker nodes (e.g. `bgp_router: "true"`). Must match labels applied to BGP router machine pools. |
| `spec.bgp.localASN` | Yes | AS number for the OCP FRR routers. From `terraform output rosa_bgp_asn`. |
| `spec.bgp.livenessDetection` | No | `bfd` or `bgp-keepalive` (default). Applies to all neighbors. BFD detects peer failure in ~1s (300ms interval × 3 multiplier). BGP keepalive detects peer failure in ~90s (default hold time). Each AZ has 2 RS endpoints, so failover on a single peer failure is automatic. |
| `spec.bgp.availabilityZones[]` | If `spec.aws` absent | Per-AZ BGP peering groups with explicit neighbor addresses. Required when `spec.aws` is absent. **Must not be set when `spec.aws` is present** — the two are mutually exclusive (CRD-enforced). |
| `spec.bgp.availabilityZones[].nodeSelector` | If `availabilityZones` set | Labels selecting BGP-enabled worker nodes in this AZ (e.g. `topology.kubernetes.io/zone`, `bgp_router_subnet`). |
| `spec.bgp.availabilityZones[].neighbors[]` | If `availabilityZones` set | BGP neighbor IPs and ASN in this AZ's subnet. |
| `spec.aws.region` | If `spec.aws` set | AWS region where the ROSA cluster and Route Server are deployed. |
| `spec.aws.routeServerIDs[]` | If `spec.aws` set | Route Server IDs for auto-discovery. The operator discovers all endpoints, their ENI addresses (BGP neighbor IPs), AZs (via subnet), and remote ASN. From `terraform output`. |

**CUDNBgpConfig status** (populated by the operator):

| Field | Description |
|:---|:---|
| `status.phase` | Current lifecycle phase: `Pending`, `Configuring`, `Ready`, or `Degraded`. |
| `status.conditions[]` | Per-phase condition details. |
| `status.aws.routeServers[]` | Discovered Route Server information (only when `spec.aws` is set). |
| `status.aws.routeServers[].routeServerID` | The Route Server ID from `spec.aws.routeServerIDs`. |
| `status.aws.routeServers[].remoteASN` | The Route Server's Amazon-side ASN (used as `remoteASN` for all BGP neighbors). |
| `status.aws.routeServers[].endpoints[]` | Discovered endpoints for this Route Server. |
| `status.aws.routeServers[].endpoints[].endpointID` | Route Server endpoint ID (e.g. `rse-0abc1111`). |
| `status.aws.routeServers[].endpoints[].availabilityZone` | AZ derived from the endpoint's subnet. |
| `status.aws.routeServers[].endpoints[].address` | ENI address of the endpoint (BGP neighbor IP). |

**CUDNBgpRouting** (application teams create per project):

| Field | Required | Description |
|:---|:---|:---|
| `spec.network.name` | Yes | Identifies the CUDN network. The operator creates a ClusterUserDefinedNetwork named `cluster-udn-<name>` that selects namespaces with label `cluster-udn: <name>`. Users must pre-create and label namespaces. |
| `spec.network.subnets[]` | Yes | CIDRs for the CUDN pod network (1 or 2 entries for single-stack or dual-stack). The operator hardcodes `topology: Layer2`, `role: Primary`, and `ipam.lifecycle: Persistent` on the generated CUDN. |

### Operator-generated resources

#### Network operator patch (from CUDNBgpConfig)

Applied regardless of whether cloud integration is configured.

```yaml
# Merge-patch applied to Network.operator.openshift.io/cluster
spec:
  additionalRoutingCapabilities:
    providers:
      - FRR
  defaultNetwork:
    ovnKubernetesConfig:
      routeAdvertisements: Enabled
```

#### FRRConfiguration (from CUDNBgpConfig — one per AZ)

The generated FRRConfigurations are identical regardless of whether cloud integration is configured. The only difference is the source of the input data: with cloud integration, neighbor addresses, remote ASN, and AZ mapping are auto-discovered from the Route Server endpoints; without cloud integration, they come from the explicit `spec.bgp.availabilityZones`. The `nodeSelector` is always `routerNodeSelector` merged with the AZ's node selector.

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: cudn-bgp-az-1
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: cudn-bgp-routing-operator
spec:
  nodeSelector:
    matchLabels:
      bgp_router: "true"
      topology.kubernetes.io/zone: us-east-1a
      bgp_router_subnet: "1"
  bgp:
    routers:
      - asn: 65001
        neighbors:
          - address: 10.0.1.47
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
          - address: 10.0.1.183
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
---
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: cudn-bgp-az-2
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: cudn-bgp-routing-operator
spec:
  nodeSelector:
    matchLabels:
      bgp_router: "true"
      topology.kubernetes.io/zone: us-east-1b
      bgp_router_subnet: "2"
  bgp:
    routers:
      - asn: 65001
        neighbors:
          - address: 10.0.2.91
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
          - address: 10.0.2.204
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
---
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: cudn-bgp-az-3
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: cudn-bgp-routing-operator
spec:
  nodeSelector:
    matchLabels:
      bgp_router: "true"
      topology.kubernetes.io/zone: us-east-1c
      bgp_router_subnet: "3"
  bgp:
    routers:
      - asn: 65001
        neighbors:
          - address: 10.0.3.62
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
          - address: 10.0.3.178
            asn: 64512
            disableMP: true
            toReceive:
              allowed:
                mode: all
```

> **Note on `disableMP: true`:** The frr-k8s CRD defaults `disableMP` to `false` when the field is omitted. The OVN-Kubernetes RouteAdvertisements controller validates it and rejects FRRConfigurations where `disableMP` is `false`. The operator therefore explicitly sets `disableMP: true` on every neighbor.

Users must pre-create namespaces with the required labels before applying a CUDNBgpRouting CR. The `k8s.ovn.org/primary-user-defined-network` label must be set at creation time — OCP admission policy prevents adding it after the namespace exists.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: app1                     # any name — does not need to match spec.network.name
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
    cluster-udn: prod            # must match spec.network.name
```

The following resources are generated from CUDNBgpRouting and are identical regardless of platform configuration:

#### ClusterUserDefinedNetwork (from CUDNBgpRouting)

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: cluster-udn-prod
  labels:
    advertise: "true"
    app.kubernetes.io/managed-by: cudn-bgp-routing-operator
spec:
  namespaceSelector:
    matchLabels:
      cluster-udn: prod
  network:
    topology: Layer2
    layer2:
      role: Primary
      ipam:
        lifecycle: Persistent
      subnets:
        - 10.100.0.0/16
```

#### RouteAdvertisements (from CUDNBgpRouting — shared)

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: cudn-bgp-route-advertisements
  labels:
    app.kubernetes.io/managed-by: cudn-bgp-routing-operator
spec:
  nodeSelector: {}
  frrConfigurationSelector: {}
  networkSelectors:
    - networkSelectionType: ClusterUserDefinedNetworks
      clusterUserDefinedNetworkSelector:
        networkSelector:
          matchLabels:
            advertise: "true"
  advertisements:
    - PodNetwork
```

---

## Controller Reconciliation

Two controllers, one per CRD.

### CUDNBgpConfig controller (singleton)

Reconciles the shared BGP infrastructure.

```
Phase 1: Patch Network Operator
  ├── Patch Network.operator.openshift.io/cluster to enable
  │   additionalRoutingCapabilities: [FRR] and routeAdvertisements: Enabled
  └── Condition: NetworkOperatorPatched
          │
          ▼
Phase 2: Wait for FRR
  ├── Watch for openshift-frr-k8s namespace to exist
  │   and frr-k8s pods to be running
  └── Condition: FRRNamespaceReady
          │
          ▼
Phase 3: Discover Route Server Infrastructure (if spec.aws configured)
  ├── Verify AWS credentials via IRSA (sts:GetCallerIdentity)
  ├── For each spec.aws.routeServerIDs[]:
  │     • DescribeRouteServers → AmazonSideAsn (remote ASN)
  │     • DescribeRouteServerEndpoints → endpoint IDs, ENI addresses, subnet IDs
  │     • DescribeSubnets → AZ for each endpoint
  ├── Build per-AZ neighbor list (endpoint address + remote ASN)
  ├── Build per-AZ endpoint ID list (for peer reconciliation)
  ├── Write discovered data to status.aws.routeServers
  └── Condition: AWSEndpointsDiscovered
          │
          ▼
Phase 4: Apply FRR Configuration (per AZ)
  ├── If spec.aws configured: use discovered AZs, neighbor IPs, and remote ASN
  │   If spec.aws not configured: use explicit spec.bgp.availabilityZones[]
  ├── For each AZ, create/update a separate FRRConfiguration CR:
  │     • nodeSelector: routerNodeSelector + topology.kubernetes.io/zone (discovery)
  │       or routerNodeSelector + AZ's explicit nodeSelector (without cloud integration)
  │     • BGP router ASN: spec.bgp.localASN
  │     • Neighbors: discovered endpoint addresses or explicit neighbor list
  │     • Peer liveness detection: spec.bgp.livenessDetection
  │     • disableMP: true (required by OVN-K RouteAdvertisements controller),
  │       toReceive.allowed.mode: all
  ├── Prune stale FRRConfigurations from previous reconciles
  │   (e.g. if AZ count was reduced)
  └── Condition: FRRConfigurationApplied
          │
          ▼
Phase 5: Reconcile AWS Resources (if spec.aws configured)
  ├── List Nodes matching spec.routerNodeSelector
  ├── For each node: read AZ from topology.kubernetes.io/zone label,
  │   private IP from status.addresses, instance ID from spec.providerID
  ├── Route Server peers: for each AZ, ensure peers exist on the
  │   discovered endpoint IDs for the local BGP-enabled worker nodes only
  ├── Source/dest check: disable on the primary ENI of each node
  └── Condition: AWSResourcesReconciled
          │
          ▼
     phase: Ready
```

**On deletion:** blocked by finalizer while any `CUDNBgpRouting` CR exists. Once all routing CRs are removed, the finalizer cleans up all FRRConfigurations and (if `spec.aws` configured) deletes all managed AWS Route Server peers. The Network operator patch (`additionalRoutingCapabilities: FRR` and `ovnKubernetesConfig.routeAdvertisements: Enabled`) is intentionally **not** reverted — disabling FRR at the cluster level could disrupt other consumers.

### CUDNBgpRouting controller (per CUDN)

Reconciles individual CUDN networks. Before executing phases, the controller runs two pre-checks in order:

1. **Duplicate network name** — `spec.network.name` must be unique across all `CUDNBgpRouting` CRs. If another CR already claims the same name, the CR is immediately set to `Degraded` with reason `DuplicateNetwork`.
2. **Config readiness** — `CUDNBgpConfig` named `cluster` must exist and be in phase `Ready`. If missing or not yet `Ready`, the routing CR remains in `Pending` phase (conditions are cleared) and requeues every 10 seconds.

```
Phase 1: Validate Namespace + Create CUDN
  ├── Validate at least one namespace exists with labels:
  │   k8s.ovn.org/primary-user-defined-network: ""
  │   cluster-udn: <spec.network.name>
  │   (if no matching namespace found → Degraded with reason NamespaceNotReady)
  ├── Create ClusterUserDefinedNetwork with:
  │   namespaceSelector matching cluster-udn: <spec.network.name>
  │   subnets from spec.network
  │   topology: Layer2, role: Primary, ipam.lifecycle: Persistent (hardcoded)
  │   label advertise: "true"
  └── Condition: CUDNCreated
          │
          ▼
Phase 2: Ensure Route Advertisements
  ├── Ensure a single shared RouteAdvertisements ("cudn-bgp-route-advertisements") exists
  │   (created on first CUDNBgpRouting reconcile, reused by all)
  │   networkSelector: advertise=true (matches all operator-managed CUDNs)
  ├── advertisements: [PodNetwork]
  └── Condition: RouteAdvertisementsCreated
          │
          ▼
     phase: Ready
```

**On deletion:** delete the owned ClusterUserDefinedNetwork. The shared RouteAdvertisements is deleted only when the last CUDNBgpRouting CR is removed.

### Status phases and error handling

Both CRs use the same phase enum. `CUDNBgpRouting` follows `Pending` → `Configuring` → `Ready`; `CUDNBgpConfig` starts directly in `Configuring` (it has no prerequisites to wait for). Either CR enters `Degraded` on errors.

| Phase | Meaning |
|:---|:---|
| `Pending` | CR accepted but prerequisites not met (e.g. `CUDNBgpConfig` not yet `Ready`) |
| `Configuring` | Reconciliation in progress, phases executing |
| `Ready` | All phases completed successfully |
| `Degraded` | A phase failed — check `status.conditions` for details |

**CUDNBgpConfig conditions:**

| Condition | Degraded Reason | Cause |
|:---|:---|:---|
| `NetworkOperatorPatched` | `PatchFailed` | Failed to patch `Network.operator.openshift.io/cluster` |
| `FRRNamespaceReady` | `CheckFailed` | Error checking FRR readiness (distinct from simply waiting) |
| `AWSEndpointsDiscovered` | `AWSCredentialsInvalid` | IRSA credentials not available or `sts:GetCallerIdentity` verification failed (check ServiceAccount annotation and IAM role trust policy) |
| `AWSEndpointsDiscovered` | `AWSDiscoveryFailed` | Failed to build the AWS platform client (e.g. Infrastructure name unavailable) or to discover Route Server endpoints (invalid Route Server ID, insufficient IAM permissions) |
| `FRRConfigurationApplied` | `ApplyFailed` | Failed to create/update one or more FRRConfigurations |
| `AWSResourcesReconciled` | `AWSReconcileFailed` | Failed to reconcile Route Server peers or disable source/dest check |

> Phase 2 also uses `FRRNamespaceReady=False` with reason `WaitingForFRR` when the FRR namespace or pods are not yet available. This is **not** `Degraded` — the CR stays in `Configuring` and requeues every 10 seconds.

**CUDNBgpRouting conditions:**

| Condition | Degraded Reason | Cause |
|:---|:---|:---|
| `CUDNCreated` | `DuplicateNetwork` | `spec.network.name` already claimed by another CUDNBgpRouting CR |
| `CUDNCreated` | `NamespaceNotReady` | No namespace found with required labels (`k8s.ovn.org/primary-user-defined-network: ""` and `cluster-udn: <name>`) |
| `CUDNCreated` | `CUDNFailed` | Failed to create/update the ClusterUserDefinedNetwork |
| `RouteAdvertisementsCreated` | `RAFailed` | Failed to ensure the shared RouteAdvertisements |

On any `Degraded` state, the controller automatically retries every 30 seconds.

### Watches and drift recovery

Both controllers watch their downstream resources for immediate drift detection. If a managed resource is deleted or modified externally, the responsible controller is notified and reconciles.

| Controller | Watches | Triggers reconcile of |
|:---|:---|:---|
| CUDNBgpConfig | `FRRConfiguration` (label-filtered) | `cluster` singleton |
| CUDNBgpConfig | `Node` (label/address/providerID changes) | `cluster` singleton |
| CUDNBgpRouting | `ClusterUserDefinedNetwork` (label-filtered) | owning `CUDNBgpRouting` CR |
| CUDNBgpRouting | `RouteAdvertisements` (label-filtered) | all `CUDNBgpRouting` CRs |

Only resources labeled `app.kubernetes.io/managed-by: cudn-bgp-routing-operator` trigger reconciliation. In addition, both controllers re-reconcile every 5 minutes in the `Ready` state as a safety-net backstop.

Inspect the failing condition for the root cause:

```bash
oc get cudnbgpconfig cluster -o jsonpath='{.status.conditions}' | jq .
oc get cudnbgprouting cudn1 -o jsonpath='{.status.conditions}' | jq .
```

---

## Development

### Prerequisites

- Go 1.24+ installed locally. Some of the build happens outside of a container.
- operator-sdk v1.42+
- `oc` CLI logged into an OCP 4.21+ cluster
- Podman (for image builds only)

### Clone the repo

```bash
git clone https://github.com/jingczhang/rosa-bgp-operator.git
cd rosa-bgp-operator
```

### Deploy and test

The operator can be tested on any OCP 4.21+ cluster with an external BGP router — with or without cloud integration.

1. Ensure the internal image registry is enabled and exposed.

   Check whether it is currently enabled (by default, it is enabled on ROSA):

```bash
oc get configs.imageregistry.operator.openshift.io cluster -o jsonpath='{.spec.managementState}'
```

   If it returns `Removed` (common on compact/SNO deployments), enable it:

```bash
oc patch configs.imageregistry.operator.openshift.io cluster --type merge -p '{"spec":{"managementState":"Managed","storage":{"emptyDir":{}}}}'
oc wait --for=condition=Available configs.imageregistry.operator.openshift.io cluster --timeout=120s
```

   Check whether it currently is exposing a default route to outside of the cluster (by default, it is not on ROSA):

```bash
oc patch configs.imageregistry.operator.openshift.io cluster --type merge -p '{"spec":{"defaultRoute":true}}'
oc wait route/default-route -n openshift-image-registry --for=jsonpath='{.status.ingress[0].conditions[0].status}'=True --timeout=120s
```

2. Build the image:

```bash
REGISTRY=$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}')
IMG=$REGISTRY/openshift-cudn-bgp-routing/operator:dev
make docker-build CONTAINER_TOOL=podman IMG=$IMG
```

3. Push the image and deploy:

```bash
oc create namespace openshift-cudn-bgp-routing --dry-run=client -o yaml | oc apply -f -
oc create sa registry-push -n openshift-cudn-bgp-routing 2>/dev/null
oc adm policy add-role-to-user registry-editor -z registry-push -n openshift-cudn-bgp-routing
podman login $REGISTRY --tls-verify=false -u unused -p $(oc create token registry-push -n openshift-cudn-bgp-routing)
make docker-push CONTAINER_TOOL=podman IMG=$IMG
make deploy IMG=image-registry.openshift-image-registry.svc:5000/openshift-cudn-bgp-routing/operator:dev
```

4. Re-deploy after code changes (rebuild, push, and restart):

```bash
make docker-build CONTAINER_TOOL=podman IMG=$IMG
make docker-push CONTAINER_TOOL=podman IMG=$IMG
oc patch deployment openshift-cudn-bgp-routing-controller-manager -n openshift-cudn-bgp-routing \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"Always"}]}}}}'
oc rollout restart deployment/openshift-cudn-bgp-routing-controller-manager -n openshift-cudn-bgp-routing
```

5. Create the CRs for your environment:

   **For ROSA HCP:** provision AWS infrastructure first with [rosa-bgp Terraform](https://github.com/msemanrh/rosa-bgp), set up the IRSA IAM role (see [AWS authentication](#aws-authentication-irsa)), then create the CR with Route Server IDs and BGP ASN from `terraform output`. The operator auto-discovers all Route Server endpoints, neighbor IPs, and remote ASN:

   ```bash
   $EDITOR config/samples/networking_v1alpha1_cudnbgpconfig.yaml # Add needed Terraform outputs
   oc apply -f config/samples/networking_v1alpha1_cudnbgpconfig.yaml
   ```

   **Without cloud integration:** create the CRs with your BGP router's ASN, neighbor addresses, and node selectors. Omit the `spec.aws` section and provide explicit `spec.bgp.availabilityZones`. See the commented-out section in `config/samples/networking_v1alpha1_cudnbgpconfig.yaml` for an example:

   ```bash
   oc apply -f your-cudnbgpconfig.yaml    # no spec.aws, explicit availabilityZones
   ```

   Then create a labeled namespace:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: app1
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
    cluster-udn: prod
EOF
```
   Finally apply the routing CR:

```bash
oc apply -f config/samples/networking_v1alpha1_cudnbgprouting.yaml
```

6. Verify:

```bash
oc get cudnbgpconfig cluster -o yaml   # phase: Ready
oc get cudnbgprouting cudn1 -o yaml    # phase: Ready
oc get frrconfiguration -n openshift-frr-k8s
oc get clusteruserdefinednetwork
oc get routeadvertisements
```

7. Clean up (delete CRs first so finalizers can clean up AWS resources and FRRConfigurations):

```bash
oc delete cudnbgprouting --all
oc delete cudnbgpconfig cluster
make undeploy
```

---

## Automated Testing

| Target | Scope | Prerequisites |
|:---|:---|:---|
| `make test` | Platform-independent unit tests (controllers + helpers) | None |
| `make test-aws` | AWS platform unit tests (mocked EC2/STS) | None |
| `make test-e2e <profile>` | E2E (BGP session verification + drift recovery), profile required | Cluster + external BGP peer |
| `make test-e2e-aws <profile>` | AWS E2E (full reconciliation lifecycle), profile required | Cluster + AWS credentials |

E2E tests read CR manifests from `test/e2e/manifests/<profile>/` and require a profile name. Shared E2E tests use CRs with explicit `availabilityZones` (no `spec.aws`); AWS E2E tests require `spec.aws`. To test your own cluster, create a profile directory with your CRs.

For full details see [docs/test-strategy.md](docs/test-strategy.md).

---

## KubeVirt VM Testing

KubeVirt VMIs in UDN-enabled namespaces **must** use `bridge` binding, not `masquerade`.

The `masquerade` binding installs nftables rules in the virt-launcher pod that drop inbound traffic on UDN interfaces (`ovn-udn1`). This blocks both IPv4 and IPv6 connectivity to the VM from other pods and external hosts.

VM spec (relevant sections):

```yaml
spec:
  domain:
    devices:
      interfaces:
        - name: default
          bridge: {}
  networks:
    - name: default
      pod: {}
```

| Binding | Inbound to VM (UDN) | Outbound from VM |
|---------|---------------------|------------------|
| masquerade | Blocked by nftables | Works |
| bridge | Works | Works |

For VM migration test using Migration Toolkit for Virtualization (MTV) see [docs/test-mtv-cudn.md](docs/test-mtv-cudn.md).
