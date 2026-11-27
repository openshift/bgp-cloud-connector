# Custom Resource Definitions

Two CRDs with clear separation of concerns:

- **`BGPCloudConfiguration`** (singleton, cluster-scoped, **must be named `cluster`**) — shared BGP infrastructure. Owned by cluster admin.
- **`BGPRouting`** (one per network, cluster-scoped) — declares a single network to advertise via BGP. Owned by application teams.

See [reconciliation.md](reconciliation.md) for how the controllers act on these resources.

## BGPCloudConfiguration (singleton — without cloud integration)

Under `platform: Manual`, `spec.bgp.peerGroups` is required: you provide explicit neighbour addresses and the node selector for each group. See [manual-platform.md](manual-platform.md) for when and why to use this mode.

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: Manual                  # you declare the peering below
  routerNodeSelector:
    bgp_router: "true"              # must match labels on BGP router machine pools

  bgp:
    localASN: 65001                  # terraform output rosa_bgp_asn
    livenessDetection: bgp-keepalive # bfd | bgp-keepalive (default)
    peerGroups:
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1a
          bgp_router_subnet: "1"
        neighbors:
          - address: 10.0.1.47       # terraform output vpc1-rs1-subnet1-ep1_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
          - address: 10.0.1.183      # terraform output vpc1-rs1-subnet1-ep2_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1b
          bgp_router_subnet: "2"
        neighbors:
          - address: 10.0.2.91       # terraform output vpc1-rs1-subnet2-ep1_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
          - address: 10.0.2.204      # terraform output vpc1-rs1-subnet2-ep2_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
      - nodeSelector:
          topology.kubernetes.io/zone: us-east-1c
          bgp_router_subnet: "3"
        neighbors:
          - address: 10.0.3.62       # terraform output vpc1-rs1-subnet3-ep1_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
          - address: 10.0.3.178      # terraform output vpc1-rs1-subnet3-ep2_ip
            remoteASN: 64512         # example only; use the Route Server ASN from Terraform
```

## BGPCloudConfiguration (singleton — with cloud integration)

`spec.platform` selects the cloud (`AWS`, `Azure`, or `GCP`), and exactly one matching block is required: each platform requires its own block (`spec.aws` / `spec.azure` / `spec.gcp`) and refuses the others; `spec.bgp.peerGroups` is permitted only under `platform: Manual`. All of this is enforced by CEL rather than by documentation. On a cloud the operator auto-discovers the Route Server / Cloud Router, BGP neighbour addresses, and remote ASN, and writes the resulting peering plan to `status.peerGroups`.

> **Peer groups differ by cloud.** AWS endpoints are per subnet, so discovery yields **one peer group per availability zone** (one FRRConfiguration each). Azure and GCP present a single regional Route Server / Cloud Router to every node, so discovery yields a **single region-wide peer group** (one FRRConfiguration covering all router nodes).

### AWS

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: AWS                     # requires spec.aws; the peering is discovered
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

### Azure

The addresses and remote ASN are read from the Route Server (which Azure models as a Virtual Hub), so there is nothing per-endpoint to enumerate. See [azure-authentication.md](azure-authentication.md) for the credentials it needs.

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: Azure                   # requires spec.azure; the peering is discovered
  routerNodeSelector:
    bgp_router: "true"

  bgp:
    localASN: 65001
    livenessDetection: bgp-keepalive

  azure:
    subscriptionID: <subscription-id>
    resourceGroup: <resource-group>
    routeServerName: <route-server-name>   # Azure Route Server (a Virtual Hub)
```

### GCP

Node peering requires NCC router-appliance spoke membership, so `spec.gcp.ncc` is part of the config, not just the Cloud Router.

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: GCP                     # requires spec.gcp; the peering is discovered
  routerNodeSelector:
    bgp_router: "true"

  bgp:
    localASN: 65001
    livenessDetection: bgp-keepalive

  gcp:
    project: <project-id>
    region: <region>
    cloudRouterName: <cloud-router-name>    # must NOT be the installer's Cloud NAT router
    ncc:
      hubName: <ncc-hub-name>
      spokePrefix: <spoke-name-prefix>
      # siteToSiteDataTransfer: false
    # enableNestedVirtualization: true      # default true; enabling restarts the instance
```

### Forwarding-only nodes (optional, any cloud platform)

When some nodes send CUDN-sourced traffic but should not become BGP speakers, add `spec.forwardingNodeSelector`. The selector is a simple label map — a node must carry **all** listed labels to match (AND semantics), just like `routerNodeSelector`.

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPCloudConfiguration
metadata:
  name: cluster
spec:
  platform: AWS
  routerNodeSelector:
    bgp_router: "true"              # these nodes speak BGP and get forwarding
  forwardingNodeSelector:
    cudn-forwarding: "true"         # these nodes get forwarding only, no peering

  bgp:
    localASN: 65001
  aws:
    region: us-east-1
    routeServerIDs:
      - rs-0abc123456789abcd
```

Behaviour summary:

- A node matching **only** `routerNodeSelector` is a BGP speaker and gets cloud forwarding enabled.
- A node matching **only** `forwardingNodeSelector` gets cloud forwarding enabled but is **not** peered.
- A node matching **both** selectors remains a speaker — it is never downgraded to forwarding-only.
- An **unset** `forwardingNodeSelector`, or one that matches no nodes, is equivalent: only speakers are updated.
- An **empty map** (`forwardingNodeSelector: {}`) is rejected by CEL validation, because it would match every node.
- `forwardingNodeSelector` **must be absent** when `platform` is `Manual` (no cloud reconciliation).
- Forwarding is **never reverted**: removing a node from the selector, or unsetting the field, leaves the cloud-side flag enabled. The flag is harmless once set.

Watch the `CompleteNodeInventory` condition to confirm the operator sees the forwarding nodes. A forwarding node missing `spec.providerID` or `topology.kubernetes.io/zone` is reported as incomplete, just as an incomplete speaker would be.

## BGPRouting (one per network)

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPRouting
metadata:
  name: cudn1
spec:
  network:
    name: prod                       # selects namespaces with label cluster-udn=prod
    subnets:
      - 10.100.0.0/16
```

For dual-stack networks, specify both an IPv4 and an IPv6 subnet:

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPRouting
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

## CRD field reference

**BGPCloudConfiguration** (cluster admin creates once):

| Field | Required | Description |
|:---|:---|:---|
| `spec.platform` | Yes | Which platform to reconcile against: `AWS`, `Azure`, `GCP`, or `Manual`. `Manual` is not a cloud: it reconciles nothing and takes its peering from `spec.bgp.peerGroups`. Selects which cloud block is required (`spec.aws` / `spec.azure` / `spec.gcp`), and refuses the others (CEL-enforced). |
| `spec.routerNodeSelector` | Yes | Cluster-wide label selector for all BGP-enabled worker nodes (e.g. `bgp_router: "true"`). Must match labels applied to BGP router machine pools. |
| `spec.forwardingNodeSelector` | No | Nodes that send packets sourced from a CUDN address. Speakers from `routerNodeSelector` are always included. Nodes that match only this field are not peered. Unset means speakers only. Forwarding is never turned back off: a node that leaves the selector, or unsetting the field, keeps it enabled. An empty map is rejected. Must be absent when `platform` is `Manual`. |
| `spec.bgp.localASN` | Yes | AS number for the OCP FRR routers. From `terraform output rosa_bgp_asn`. |
| `spec.bgp.livenessDetection` | No | `bfd` or `bgp-keepalive` (default). Applies to all neighbors. BFD detects peer failure in ~1s (300ms interval × 3 multiplier). BGP keepalive detects peer failure in ~90s (default hold time). Whether a single peer failure fails over automatically depends on the platform having a second neighbour in the group to fall back to; on AWS each AZ has 2 Route Server endpoints, so it does. |
| `spec.bgp.peerGroups[]` | Under `platform: Manual` | BGP peer groups with explicit neighbour addresses. Required under `platform: Manual`, and must not be set on any other platform, where the groups are discovered (CEL-enforced). |
| `spec.bgp.peerGroups[].nodeSelector` | If `peerGroups` set | Labels selecting the BGP-enabled worker nodes in this group (e.g. `topology.kubernetes.io/zone`, `bgp_router_subnet`). |
| `spec.bgp.peerGroups[].neighbors[]` | If `peerGroups` set | The BGP neighbour IPs and ASN this group's nodes peer with. |
| `spec.bgp.peerGroups[].neighbors[].address` | Yes | The neighbour's IP address. |
| `spec.bgp.peerGroups[].neighbors[].remoteASN` | Yes | The neighbour's AS number. |
| `spec.bgp.peerGroups[].neighbors[].ebgpMultiHop` | No | Set when the neighbour is not on the node's link (e.g. an off-link route reflector). Cloud platforms set this automatically where needed; under `platform: Manual` you set it yourself. |
| `spec.aws.region` | Under `platform: AWS` | AWS region where the ROSA cluster and Route Server are deployed. |
| `spec.aws.routeServerIDs[]` | Under `platform: AWS` | Route Server IDs for auto-discovery. The operator discovers all endpoints, their ENI addresses (BGP neighbor IPs), AZs (via subnet), and remote ASN. From `terraform output`. |
| `spec.azure.subscriptionID` | Under `platform: Azure` | Azure subscription ID. |
| `spec.azure.resourceGroup` | Under `platform: Azure` | Resource group holding the Route Server. |
| `spec.azure.routeServerName` | Under `platform: Azure` | The Azure Route Server (modeled as a Virtual Hub). Its `virtualRouterIps` become the BGP neighbours and its `virtualRouterAsn` the remote ASN — read from it, not declared. Azure fixes the far-side ASN. |
| `spec.gcp.project` | Under `platform: GCP` | GCP project ID. |
| `spec.gcp.region` | Under `platform: GCP` | GCP region. |
| `spec.gcp.cloudRouterName` | Under `platform: GCP` | The Cloud Router the router nodes peer with; its interface addresses become the BGP neighbours and its `bgp.asn` the remote ASN. Must **not** be the installer's Cloud NAT router (no interfaces, carries cluster egress). |
| `spec.gcp.ncc.hubName` | Under `platform: GCP` | Network Connectivity Center hub the router nodes attach to as spokes. Spoke membership is a precondition of peering. |
| `spec.gcp.ncc.spokePrefix` | Under `platform: GCP` | Name prefix for the operator-managed NCC spokes. Spokes are numbered `<prefix>-0`, `<prefix>-1`, … at ≤8 router-appliance instances each. |
| `spec.gcp.ncc.siteToSiteDataTransfer` | No | Enable NCC site-to-site data transfer on the managed spokes. Part of the spoke drift check. |
| `spec.gcp.enableNestedVirtualization` | No (default `true`) | Enable nested virtualization on the router instances (needed by KubeVirt). **Enabling restarts the instance.** |

**BGPCloudConfiguration status** (populated by the operator):

| Field | Description |
|:---|:---|
| `status.phase` | Current lifecycle phase: `Pending`, `Configuring`, `Ready`, or `Degraded`. |
| `status.conditions[]` | Per-phase condition details. |
| `status.peerGroups[]` | The peering plan the operator discovered and rendered into FRRConfigurations. |
| `status.peerGroups[].key` | Names the group in cloud terms: the availability zone on AWS. |
| `status.peerGroups[].nodeSelector` | Narrows `spec.routerNodeSelector` to this group's nodes. |
| `status.peerGroups[].neighbors[]` | The addresses this group's router nodes peer with. |

**BGPRouting** (application teams create one per network):

| Field | Required | Description |
|:---|:---|:---|
| `spec.network.name` | When provisioning | Identifies the CUDN network. The operator creates a ClusterUserDefinedNetwork named `cluster-udn-<name>` that selects namespaces with label `cluster-udn: <name>`. Users must pre-create and label namespaces. |
| `spec.network.subnets[]` | When provisioning | CIDRs for the CUDN pod network (1 or 2 entries for single-stack or dual-stack). The operator hardcodes `topology: Layer2`, `role: Primary`, and `ipam.lifecycle: Persistent` on the generated CUDN. |
| `spec.networkRef.kind` | When referencing | Must be `ClusterUserDefinedNetwork` in the `k8s.ovn.org` API group. |
| `spec.networkRef.name` | When referencing | Name of the existing primary Layer 2 or Layer 3 CUDN. |

Both modes report the CUDN in `status.networkRef` and the advertisement in
`status.routeAdvertisementsRef`. See the status examples below.

### Advertise an existing primary CUDN

Set exactly one of `spec.network` and `spec.networkRef`. The network source and
the reference are immutable. Create a ready `BGPCloudConfiguration` for the
peers, then opt the existing CUDN in as the network administrator.

```bash
name=vm-network
uid=$(oc get clusteruserdefinednetwork "$name" -o jsonpath='{.metadata.uid}')
oc label clusteruserdefinednetwork "$name" "networking.openshift.io/bgp-network-ref=$uid"
```

OVN-Kubernetes copies the label to the CUDN's generated attachments so that
`RouteAdvertisements` can select this network. Reserve this label for the named
CUDN and its generated attachments. Set the label again with the new UID if the
CUDN is recreated. Use a single advertisement configuration for the network,
and keep it outside the shared provisioning selector `advertise: "true"`.

```yaml
apiVersion: networking.openshift.io/v1beta1
kind: BGPRouting
metadata:
  name: advertise-vm-network
spec:
  networkRef:
    kind: ClusterUserDefinedNetwork
    name: vm-network
```

The controller owns a dedicated `RouteAdvertisements` that selects the
connector's FRR configuration. `RouteAdvertisementsAccepted` reports
OVN-Kubernetes acceptance for its current generation. Deleting the
`BGPRouting` withdraws its own advertisement and preserves the CUDN.

### Status examples

With `spec.network`, the connector ensures a CUDN and the shared advertisement.
The shared object reports OVN acceptance.

```yaml
status:
  phase: Ready
  observedGeneration: 1
  networkRef:
    name: cluster-udn-prod
    uid: 24b08231-6dab-4aea-bc7d-bbb0cb588e07
    role: Primary
    topology: Layer2
  routeAdvertisementsRef:
    name: bgp-cc-route-advertisements
  conditions:
    - type: Available
      status: "True"
      reason: AsExpected
    - type: NetworkCreated
      status: "True"
      reason: Created
    - type: RouteAdvertisementsCreated
      status: "True"
      reason: Created
```

With `spec.networkRef`, `Ready` reflects OVN acceptance of the current
generation of the dedicated advertisement. Check the external router to confirm
that the peer learned the route.

```yaml
status:
  phase: Ready
  observedGeneration: 1
  networkRef:
    name: vm-network
    uid: 672cf656-c787-4158-88c6-21adc0782ea5
    role: Primary
    topology: Layer2
  routeAdvertisementsRef:
    name: bgp-cc-ref-22081cab2916d4ffa2c1a94cac326393e
  conditions:
    - type: Available
      status: "True"
      reason: AsExpected
    - type: NetworkResolved
      status: "True"
      reason: Resolved
    - type: RouteAdvertisementsAccepted
      status: "True"
      reason: Accepted
```

## Operator-generated resources

### Network operator patch (from BGPCloudConfiguration)

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

### FRRConfiguration (from BGPCloudConfiguration — one per peer group)

The generated FRRConfigurations are identical regardless of whether cloud integration is configured. The only difference is the source of the input data: with cloud integration, neighbor addresses, remote ASN, and AZ mapping are auto-discovered from the Route Server endpoints; under `platform: Manual` they come from the explicit `spec.bgp.peerGroups`. The `nodeSelector` is always `routerNodeSelector` merged with the group's node selector.

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: bgp-cc-1
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: bgp-cloud-connector
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
  name: bgp-cc-2
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: bgp-cloud-connector
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
  name: bgp-cc-3
  namespace: openshift-frr-k8s
  labels:
    app.kubernetes.io/managed-by: bgp-cloud-connector
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

Users must pre-create namespaces with the required labels before applying a BGPRouting CR. The `k8s.ovn.org/primary-user-defined-network` label must be set at creation time — OCP admission policy prevents adding it after the namespace exists.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: app1                     # any name — does not need to match spec.network.name
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
    cluster-udn: prod            # must match spec.network.name
```

The following resources are generated from BGPRouting and are identical regardless of platform configuration:

### ClusterUserDefinedNetwork (from BGPRouting)

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: cluster-udn-prod
  labels:
    advertise: "true"
    app.kubernetes.io/managed-by: bgp-cloud-connector
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

### RouteAdvertisements (from BGPRouting — shared)

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: bgp-cc-route-advertisements
  labels:
    app.kubernetes.io/managed-by: bgp-cloud-connector
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
