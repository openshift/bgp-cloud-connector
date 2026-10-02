# Controller Reconciliation

Two controllers, one per CRD. See [custom-resources.md](custom-resources.md) for the resources they act on.

## BGPCloudConfiguration controller (singleton)

Reconciles the shared BGP infrastructure.

> The cloud-specific phases (3 and 5) below use **AWS** as the example. The same phases run for Azure and GCP with their equivalent operations — see [Cloud platform abstraction](#cloud-platform-abstraction).

```text
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
  ├── Verify AWS credentials (sts:GetCallerIdentity)
  ├── For each spec.aws.routeServerIDs[]:
  │     • DescribeRouteServers → AmazonSideAsn (remote ASN)
  │     • DescribeRouteServerEndpoints → endpoint IDs, ENI addresses, subnet IDs
  │     • DescribeSubnets → AZ for each endpoint
  ├── Build per-AZ neighbor list (endpoint address + remote ASN)
  ├── Build per-AZ endpoint ID list (for peer reconciliation)
  ├── Write the discovered peering plan to status.peerGroups
  └── Condition: CloudEndpointsDiscovered
          │
          ▼
Phase 4: Apply FRR Configuration (per peer group)
  ├── If spec.aws configured: use discovered AZs, neighbor IPs, and remote ASN
  │   Under platform: Manual: use explicit spec.bgp.peerGroups[]
  ├── For each peer group, create/update a separate FRRConfiguration CR:
  │     • nodeSelector: routerNodeSelector + topology.kubernetes.io/zone (discovery)
  │       or routerNodeSelector + the group's explicit nodeSelector (platform: Manual)
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
  └── Condition: CloudResourcesReconciled
          │
          ▼
     phase: Ready
```

**On deletion:** blocked by finalizer while any `BGPRouting` CR exists. Once all routing CRs are removed, the finalizer cleans up all FRRConfigurations and (on a cloud) calls the platform's `Cleanup()` to delete the peers/spokes this cluster created (forwarding fixes such as `SourceDestCheck`/`canIpForward`/`enableIPForwarding` are left in place, as they are harmless and may be shared). The Network operator patch (`additionalRoutingCapabilities: FRR` and `ovnKubernetesConfig.routeAdvertisements: Enabled`) is intentionally **not** reverted — disabling FRR at the cluster level could disrupt other consumers.

### Cloud platform abstraction

Phases 3 and 5 are provider-agnostic. The operator builds a single `CloudPlatform` implementation from `spec.platform` (AWS, Azure, or GCP) and the phases call the same `DiscoverEndpoints` (Phase 3) and `ReconcileNodes` (Phase 5) regardless of cloud — all per-cloud divergence lives behind that interface. The phase diagram above lists the **AWS** API calls as an example; the equivalent Azure (Virtual Hub / BGP connections / NIC forwarding) and GCP (Cloud Router / NCC spokes / `canIpForward`) operations are described in [cloud-integration.md](cloud-integration.md). The peer-group count discovered in Phase 3 also differs: one per AZ on AWS, a single region-wide group on Azure and GCP.

`platform: Manual` skips the cloud phases (3 and 5) entirely and takes its peering from `spec.bgp.peerGroups`.

## BGPRouting controller (per CUDN)

Reconciles individual CUDN networks. Before executing phases, the controller runs two pre-checks in order:

1. **Duplicate network name** — `spec.network.name` must be unique across all `BGPRouting` CRs. If another CR already claims the same name, the CR is immediately set to `Degraded` with reason `DuplicateNetwork`.
2. **Config readiness** — `BGPCloudConfiguration` named `cluster` must exist and be in phase `Ready`. If missing or not yet `Ready`, the routing CR remains in `Pending` phase (conditions are cleared) and requeues every 10 seconds.

```text
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
  └── Condition: NetworkCreated
          │
          ▼
Phase 2: Ensure Route Advertisements
  ├── Ensure a single shared RouteAdvertisements ("bgp-cc-route-advertisements") exists
  │   (created on first BGPRouting reconcile, reused by all)
  │   networkSelector: advertise=true (matches all operator-managed CUDNs)
  ├── advertisements: [PodNetwork]
  └── Condition: RouteAdvertisementsCreated
          │
          ▼
Phase 3: Ensure VM host routes
  ├── List running VirtualMachineInstances in selected primary-UDN namespaces
  ├── Write one FRRConfiguration per hosting router node with guest /32 or /128 prefixes
  ├── Remove stale per-node FRRConfigurations after a VM stops or moves
  └── Condition: VMHostRoutesConfigured
          │
          ▼
     phase: Ready
```

**On deletion:** delete the owned per-node VM host-route FRRConfigurations and ClusterUserDefinedNetwork. The shared RouteAdvertisements is deleted only when the last BGPRouting CR is removed.

## Status phases and error handling

Both CRs use the same phase enum. `BGPRouting` follows `Pending` → `Configuring` → `Ready`; `BGPCloudConfiguration` starts directly in `Configuring` (it has no prerequisites to wait for). Either CR enters `Degraded` on errors.

| Phase | Meaning |
|:---|:---|
| `Pending` | CR accepted but prerequisites not met (e.g. `BGPCloudConfiguration` not yet `Ready`) |
| `Configuring` | Reconciliation in progress, phases executing |
| `Ready` | All phases completed successfully |
| `Degraded` | A phase failed — check `status.conditions` for details |

**BGPCloudConfiguration conditions:**

| Condition | Reason | Cause |
|:---|:---|:---|
| `NetworkOperatorPatched` | `PatchFailed` | Failed to patch `Network.operator.openshift.io/cluster` |
| `FRRNamespaceReady` | `CheckFailed` | Error checking FRR readiness (distinct from simply waiting) |
| `CloudEndpointsDiscovered` | `CloudCredentialsInvalid` | Credentials were found but `sts:GetCallerIdentity` failed (check the IAM role trust policy, or the secret CCO wrote). This is a **terminal** condition (`platform.CredentialError`) — it does not requeue or auto-recover; correct the credentials/trust policy/permissions and trigger reconciliation again. A freshly minted IAM key that has not yet propagated fails here too and likewise will not clear on its own |
| `CloudEndpointsDiscovered` | `WaitingForCloudCredentials` | The cluster has been asked (via CCO) for credentials and has not yet provided them. This reason only appears on a cloud platform that uses CCO-provided credentials (the AWS CCO path) — never under `platform: Manual`. Not `Degraded`: the CR stays `Configuring` and requeues every 10 seconds. If it never clears on a CCO `credentialsMode: Manual` (Manual/STS) cluster, `ROLEARN` is unset -- CCO ignores a request without `stsIAMRoleARN` |
| `CloudEndpointsDiscovered` | `CloudDiscoveryFailed` | Failed to build the cloud platform client (e.g. Infrastructure name unavailable, ambient credentials unresolved) or to discover endpoints (bad Route Server / Cloud Router reference, insufficient permissions). The generic discovery failure for all clouds |
| `CloudEndpointsDiscovered` | `RouteServerNotFound` | **AWS only** — the specified Route Server ID was not found. Azure/GCP discovery failures surface as `CloudDiscoveryFailed` |
| `FRRConfigurationApplied` | `ApplyFailed` | Failed to create/update one or more FRRConfigurations |
| `CloudResourcesReconciled` | `CloudReconcileFailed` | Failed to reconcile Route Server peers or disable source/dest check |

> Phase 2 also uses `FRRNamespaceReady=False` with reason `WaitingForFRR` when the FRR namespace or pods are not yet available. This is **not** `Degraded` — the CR stays in `Configuring` and requeues every 10 seconds.

See [aws-authentication.md](aws-authentication.md#troubleshooting) for the credential-related conditions in more detail.

**BGPRouting conditions:**

| Condition | Reason | Cause |
|:---|:---|:---|
| `NetworkCreated` | `DuplicateNetwork` | `spec.network.name` already claimed by another BGPRouting CR |
| `NetworkCreated` | `NamespaceNotReady` | No namespace found with required labels (`k8s.ovn.org/primary-user-defined-network: ""` and `cluster-udn: <name>`) |
| `NetworkCreated` | `CUDNFailed` | Failed to create/update the ClusterUserDefinedNetwork |
| `RouteAdvertisementsCreated` | `RAFailed` | Failed to ensure the shared RouteAdvertisements |
| `VMHostRoutesConfigured` | `VMHostRoutesFailed` | Failed to discover VMIs or write/prune host-route FRRConfigurations |
| `VMHostRoutesConfigured` | `VMIAPIUnavailable` | KubeVirt's VMI API disappeared while host routes still exist. Existing routes are preserved; restore KubeVirt or remove confirmed stale host-route FRRConfigurations. The routing CR remains `Ready` because the network is configured. |
| `VMHostRoutesConfigured` | `VMHostRoutesIncomplete` | A VM is on a node without a matching BGP peer, or its address family has no matching peer. The routing CR remains `Ready`. |
| `VMHostRoutesConfigured` | `WaitingForVMAddresses` | A VMI has no usable address yet. The routing CR remains `Ready` and retries every 5 seconds. |

Non-terminal `Degraded` states are retried automatically every 30 seconds. Terminal reasons do **not** retry: `DuplicateNetwork` here (and `CloudCredentialsInvalid` / `RouteServerNotFound` on `BGPCloudConfiguration`) stay `Degraded` until the underlying problem is corrected and reconciliation is triggered again externally.

## Watches and drift recovery

| Controller | Watches | Triggers reconcile of |
|:---|:---|:---|
| BGPCloudConfiguration | `Node` (label/address/providerID changes) | `cluster` singleton |
| BGPRouting | `ClusterUserDefinedNetwork` (label-filtered) | owning `BGPRouting` CR |
| BGPRouting | `Node` label changes; `BGPCloudConfiguration` BGP changes; `BGPRouting` network-name changes | all routing CRs |
| BGPRouting | `VirtualMachineInstance` when KubeVirt exists at startup; otherwise `kubevirt.io=virt-launcher` Pods | routing CR for the workload's primary-UDN namespace |

The `app.kubernetes.io/managed-by: bgp-cloud-connector` label filter applies to generated ClusterUserDefinedNetworks. The BGPCloudConfiguration controller watches BGP-relevant Node labels, addresses, and provider IDs; the BGPRouting controller watches Node label changes. When KubeVirt is absent at startup, the fallback Pod informer has a server-side `kubevirt.io=virt-launcher` selector. Pods read for FRR readiness bypass the cache. Both controllers also reconcile every 5 minutes in the `Ready` state.

`FRRConfiguration` and `RouteAdvertisements` are deliberately not watched. Neither CRD exists until the operator patches the Network operator, so watching them would mean the manager could only start on a cluster where its own work had already been done: it would fail to sync those caches and exit. Drift on the `FRRConfigurations` and the shared `RouteAdvertisements` the operator writes is corrected at the next resync instead, so it is noticed within `--resync-interval` rather than immediately.

Inspect the failing condition for the root cause:

```bash
oc get bgpcloudconfiguration cluster -o jsonpath='{.status.conditions}' | jq .
oc get bgprouting cudn1 -o jsonpath='{.status.conditions}' | jq .
```
