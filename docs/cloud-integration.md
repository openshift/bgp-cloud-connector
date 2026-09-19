# Cloud platform integration

BGP peering between OCP worker nodes and a cloud BGP service (e.g. AWS VPC Route Server) requires configuration on **both sides**:

- **OCP side:** BGP-enabled worker nodes *initiate* BGP sessions toward the cloud BGP service endpoints.
- **Cloud side:** The cloud BGP service must be told to *accept* sessions from each node's IP and ASN.

When a BGP-enabled worker node is replaced (upgrade, spot termination, scaling), two things break at the cloud layer:

1. **Stale BGP peers** — the cloud BGP service still has a peer registered for the old node's IP. The new node cannot establish a session because it has no peer registration.
2. **Forwarding rules revert** — the new node's network interface defaults to dropping forwarded traffic (e.g. AWS `SourceDestCheck=true`), silently breaking all CUDN pod traffic through that node.

Without cloud platform integration, these require manual intervention (e.g. re-running `terraform apply`). With cloud platform integration, the operator creates and reconciles the cloud-side BGP peering and traffic forwarding for node changes.

Kubernetes has out-of-tree cloud controller managers (e.g. [cloud-provider-aws](https://github.com/kubernetes/cloud-provider-aws)) that implement a broad `cloudprovider.Interface`. This operator uses platform API (e.g. `aws-sdk-go-v2`) directly instead because it only needs a narrow slice of platform functionality (Route Server peers + SourceDestCheck). Importing a full cloud controller manager would bring a large dependency graph for minimal benefit. The architectural pattern (interface-based, per-provider package) is the same. The operator gets its AWS credentials from the cluster, by asking the Cloud Credential Operator for its own (the recommended path), or from the pod where a webhook has already injected them — see [AWS authentication](aws-authentication.md). Credential lifecycle depends on the mode: CCO **Mint** and **passthrough** provide an IAM user access-key pair stored in the `credentials` Secret, static until rotated or revoked; **Manual/STS** provides temporary STS role credentials; and **IRSA** provides temporary web-identity role credentials.

## AWS platform

When AWS platform integration is configured, the operator performs additional actions during `BGPCloudConfiguration` reconciliation:

| Action | AWS API calls | Trigger |
|:---|:---|:---|
| Verify credentials | `sts:GetCallerIdentity` | Every reconcile (before any EC2 calls); credentials resolved as described in [AWS authentication](aws-authentication.md) |
| Discover Route Server infrastructure | `DescribeRouteServers`, `DescribeRouteServerEndpoints`, `DescribeSubnets` | Every reconcile (before FRR configuration) |
| Reconcile Route Server peers | `DescribeRouteServerPeers`, `CreateRouteServerPeer`, `DeleteRouteServerPeer`, `CreateTags` | BGP-enabled worker node added, removed, or IP changed |
| Disable SourceDestCheck | `DescribeInstances`, `ModifyNetworkInterfaceAttribute` | New BGP-enabled worker node detected |

**Auto-discovery:** The operator discovers Route Server endpoints, their ENI addresses (BGP neighbor IPs), availability zones, and the Route Server's remote ASN automatically from the provided Route Server IDs. The user does not need to specify per-AZ endpoint IDs or neighbor addresses — the operator derives them via `DescribeRouteServerEndpoints` (endpoint ID + ENI address + subnet), `DescribeSubnets` (subnet → AZ mapping), and `DescribeRouteServers` (remote ASN). The peering plan it arrives at is written to `status.peerGroups` for observability. This also drives FRR configuration generation — the operator creates one FRRConfiguration per discovered AZ with the discovered neighbor addresses.

Route Server peers are created **per-AZ** — each BGP-enabled worker node is peered with its local AZ's Route Server endpoints. Peers are tagged with `managed-by: bgp-cloud-connector/<infrastructureName>` for lifecycle management, where `<infrastructureName>` is read automatically from the OpenShift `Infrastructure/cluster` object (`status.infrastructureName`). This cluster-scoped tag ensures multiple clusters sharing the same VPC Route Server do not interfere with each other's peers. If a peer already exists at a desired IP but was not created by the operator (e.g. created manually or by Terraform), the operator adopts it by adding the `managed-by` tag rather than attempting to create a duplicate.

## Azure platform

Azure models a Route Server as a **Virtual Hub**. The actions the operator performs during `BGPCloudConfiguration` reconciliation:

| Action | Azure API | Trigger |
|:---|:---|:---|
| Discover neighbours | `VirtualHubsClient.Get` — reads `virtualRouterIps` (neighbours) and `virtualRouterAsn` (remote ASN) | Every reconcile |
| Enable IP forwarding | `InterfacesClient` read-modify-write of `enableIPForwarding` on the router-node NIC | New BGP-enabled worker node detected (done **before** peering) |
| Reconcile Route Server BGP connections | `VirtualHubBgpConnectionClient.BeginCreateOrUpdate` / `BeginDelete` | BGP-enabled worker node added, removed, or IP changed |

Azure-specific behaviour:

- **One region-wide peer group.** An Azure Route Server is regional and presents the same redundant pair of addresses to every node, so discovery produces a single peer group (one FRRConfiguration) — not per-AZ like AWS.
- **eBGP multi-hop is mandatory.** The Route Server sits in its own subnet rather than on the node's link, so every neighbour is configured with eBGP multi-hop.
- **The far-side ASN is fixed** by the hub and is not configurable.
- **Ownership is by name, not tags.** Azure BGP connections carry no tags, so the connection **name** (`<infrastructureName>-bgp-<node-ip>`) is the lifecycle marker; connections that don't match are left alone (other clusters' peerings are preserved). Names are keyed on the node IP for session stability and capped at 80 characters.
- **Cross-resource-group interfaces.** The router-node NICs and the Route Server can be reachable by different identities (the ARO case). `spec.azure.networkInterfaceClientID` names a separate managed identity used for NIC calls only — see [Azure authentication](azure-authentication.md).

## GCP platform

GCP peers nodes with a **Cloud Router**, but only after each node joins a **Network Connectivity Center (NCC)** router-appliance spoke. The actions:

| Action | GCP API | Trigger |
|:---|:---|:---|
| Discover neighbours | `RoutersService.Get` — reads Cloud Router interface `ipRange`s (neighbours) and `bgp.asn` (remote ASN) | Every reconcile |
| Set `canIpForward` | `InstancesService.Get` + `Instances.Update` (non-disruptive `REFRESH`) | New BGP-enabled worker node detected |
| Enable nested virtualization | `Instances.Update` (`RESTART` — **restarts the instance**) | New node, when `spec.gcp.enableNestedVirtualization` (default true) |
| Reconcile NCC spokes | NCC `Spoke` get/create/patch/delete | Node set changes — done **before** peering (spoke membership is a precondition) |
| Reconcile Cloud Router peers | `Routers.Update` (patches the whole peer list) | BGP-enabled worker node added, removed, or IP changed |

GCP-specific behaviour:

- **One region-wide peer group.** Every node peers with the same Cloud Router interface addresses, so discovery produces a single peer group (one FRRConfiguration).
- **`disable-connected-check` is required.** The Cloud Router is off-link, so the operator injects a raw FRR `neighbor <ip> disable-connected-check` directive; FRR would otherwise reject the neighbour as unreachable.
- **NCC spoke membership precedes peering.** A VM cannot peer with a Cloud Router until it belongs to a router-appliance spoke. Spokes are numbered `<spokePrefix>-0`, `<spokePrefix>-1`, … at **≤8 instances each**; surplus numbered spokes are pruned.
- **The Cloud Router must not be the installer's Cloud NAT router** — that router has no interfaces and carries cluster egress; discovery fails against it.
- **Ownership is by name/description.** Cloud Router peers can't carry labels, so the peer **name** (`<clusterName>-bgp-…`) marks ownership; spokes are matched by numbered name prefix and a managed-by description. Other clusters' peers are preserved.
- **Nested virtualization restarts the instance** — enabling it is disruptive by design (KubeVirt needs it).

## Multi-cloud support

AWS, Azure, and GCP platform integration are all implemented (plus `platform: Manual` for no cloud reconciliation). Each cloud is an interface-based, per-provider package under `internal/platform/`, mapping the same concerns to provider-specific concepts:

| Concern | AWS | Azure | GCP |
|:---|:---|:---|:---|
| Endpoint discovery | DescribeRouteServerEndpoints + DescribeSubnets | Azure Route Server (Virtual Hub) IP config | Cloud Router interface listing |
| BGP peering | VPC Route Server peers | Azure Route Server BGP connections | Cloud Router peers (+ NCC spoke membership) |
| Forwarding fix | SourceDestCheck=false | IP forwarding enabled on the NIC | canIpForward=true |
| Identity | IRSA / CCO (see [AWS auth](aws-authentication.md)) | Workload Identity (see [Azure auth](azure-authentication.md)) | Workload Identity Federation / ADC (see [GCP auth](gcp-authentication.md)) |

Authentication differs by cloud: AWS can have the operator provision credentials via CCO, whereas Azure and GCP rely on an ambient identity supplied to the pod. See each cloud's authentication page for details.

## What the operator replaces

| Capability | PoC | Operator |
|:---|:---|:---|
| Enable FRR + routeAdvertisements | `oc patch` in shell script (step 6) | Controller patches Network CR on reconcile |
| Wait for FRR readiness | `sleep 60`, retry on error (step 6) | Controller polls FRR namespace and pods, requeues every 10s until ready |
| FRR BGP configuration | Single `FRRConfiguration` inline in script with all 6 neighbors hard-coded from `terraform output` — cross-AZ sessions fail (step 6) | One `FRRConfiguration` per AZ, neighbors and ASN auto-discovered from Route Server APIs |
| Namespace + CUDN | `oc apply -f yamls/` (step 7) | User creates labeled namespaces; controller creates ClusterUserDefinedNetwork per BGPRouting CR |
| RouteAdvertisements | `oc apply -f yamls/` (step 7) | Controller ensures a single shared RouteAdvertisements |
| Route Server peers | Terraform creates all peers statically (step 4) | Controller creates per-AZ peers dynamically, adopts/removes on node changes |
| Source/dest check | Terraform disables via shell script (step 4) | Controller disables on each BGP-enabled worker node's primary ENI |

The [rosa-bgp PoC](https://github.com/msemanrh/rosa-bgp) is the manual baseline this operator automates.
