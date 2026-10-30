# BGP cloud connector

Kubernetes operator for OpenShift that automates L3 direct routing between CUDN Pod networks and external networks via BGP. Replaces the manual in-cluster steps from the [rosa-bgp PoC](https://github.com/msemanrh/rosa-bgp).

The operator is **cloud platform aware**. When platform configuration is provided (e.g. AWS), the operator auto-discovers cloud BGP infrastructure (Route Server endpoints, neighbor IPs, remote ASN, AZ mapping) and manages cloud-side networking resources (Route Server peers, SourceDestCheck) to keep BGP peering and traffic forwarding current for node changes.

```
┌──────────────────────────────────────────────────────────────────────┐
│  Cloud Infrastructure (Terraform provisions once)                    │
│  VPC / subnets / route server / TGW / etc.                           │
│  Terraform outputs: Route Server IDs, local BGP ASN, AWS region      │
└──────────────────────────────────┬───────────────────────────────────┘
                                   │ user copies terraform outputs into CR spec
                                   ▼
┌──────────────────────────────────────────────────────────────────────┐
│  In-Cluster (Operator)                                               │
│  BGPCloudConfiguration CR (singleton — BGP infra)                    │
│  └── enables FRR, discovers cloud endpoints, reconciles peering      │
│  BGPRouting CR (one per network)                                     │
│  └── ClusterUserDefinedNetwork + shared RouteAdvertisements          │
└──────────────────────────────────────────────────────────────────────┘
```

## Documentation

| Doc | What it covers |
|:---|:---|
| [Architecture](docs/architecture.md) | The two-layer (cloud + in-cluster) model and the CRD ownership split. |
| [Cloud platform integration](docs/cloud-integration.md) | How the operator peers with a cloud BGP service, per-cloud actions, auto-discovery, and the AWS/Azure/GCP mapping. |
| [AWS authentication](docs/aws-authentication.md) | AWS credentials — the recommended CCO path and the IRSA alternative. |
| [Azure authentication](docs/azure-authentication.md) | Azure credentials: passthrough, ARO with a service principal, and Workload Identity. |
| [GCP authentication](docs/gcp-authentication.md) | GCP credentials — Workload Identity Federation / Application Default Credentials. |
| [Manual platform](docs/manual-platform.md) | `platform: Manual` — bring your own BGP router with explicit peer groups, no cloud integration. |
| [Direct route advertisements](config/samples/direct-route-advertisements/README.md) | Complete example using a user-managed CUDN and `RouteAdvertisements`, without `BGPRouting`. |
| [Custom resources](docs/custom-resources.md) | `BGPCloudConfiguration` and `BGPRouting` reference, field tables, and operator-generated resources. |
| [Controller reconciliation](docs/reconciliation.md) | Reconciliation phases, status conditions, watches, and drift recovery. |
| [Development and deployment](docs/deployment.md) | Build, deploy, and test the operator; OLM bundle install; OLM packaging. |
| [Releasing](docs/releasing.md) | Product minor setup, then the version, stage, and production release steps. |
| [Test strategy](docs/test-strategy.md) | Test layers, make targets, and per-area test plans. |
| [KubeVirt VM testing](docs/kubevirt-testing.md) | UDN binding requirements for VMs and MTV migration testing. |

## Quick start

Requires an OCP 4.21+ cluster. See [Development and deployment](docs/deployment.md) for the full flow (image build, registry setup, cleanup).

1. **Deploy the operator** (from a published image or a locally built one — see the deployment doc).
2. **On a cloud platform, set up credentials** — [AWS](docs/aws-authentication.md), [Azure](docs/azure-authentication.md), or [GCP](docs/gcp-authentication.md). (Skip for `platform: Manual`.)
3. **Create the shared BGP configuration** (`BGPCloudConfiguration`, singleton named `cluster`) — set `spec.platform` and the matching cloud block (`spec.aws` / `spec.azure` / `spec.gcp`), or `platform: Manual` with explicit `spec.bgp.peerGroups`:

   ```bash
   $EDITOR config/samples/networking_v1beta1_bgpcloudconfiguration.yaml
   oc apply -f config/samples/networking_v1beta1_bgpcloudconfiguration.yaml
   ```

4. **Create a labeled namespace and a routing CR** (`BGPRouting`, one per network):

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
   oc apply -f config/samples/networking_v1beta1_bgprouting.yaml
   ```

5. **Verify** both CRs reach `phase: Ready`:

   ```bash
   oc get bgpcloudconfiguration cluster -o jsonpath='{.status.phase}'
   oc get bgprouting cudn1 -o jsonpath='{.status.phase}'
   ```

See [Custom resources](docs/custom-resources.md) for the CR schema and the `platform: Manual` (no cloud integration) variant.
