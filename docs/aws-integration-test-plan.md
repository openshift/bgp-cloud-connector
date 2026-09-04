# AWS Test Plan

Automated test plan for the BGP cloud connector's AWS platform integration.

- [Test Configuration](#test-configuration)
- [Unit Tests (Mocked EC2)](#unit-tests-mocked-ec2)
- [E2E Tests (ROSA HCP Cluster)](#e2e-tests-rosa-hcp-cluster)
- [How to Run](#how-to-run)

## Test Configuration

**Unit tests** hardcode values based on the rosa-bgp PoC configuration (mocked EC2/STS clients, no AWS credentials or cluster required):

| Field | Value |
|:---|:---|
| Region | us-east-1 |
| Availability Zones | us-east-1a, us-east-1b, us-east-1c |
| Local BGP ASN | 65001 |
| Remote BGP ASN | 64512 (Route Server ASN, auto-discovered) |
| Route Server IDs | 1 (with 2 endpoints per AZ, 6 total, auto-discovered) |
| Liveness detection | bfd |
| BGP router nodes | 1 per AZ (3 total), labeled `bgp_router: "true"` |
| CUDN network | name=`prod`, CIDR from Terraform outputs |

The default credential chain (IRSA) is bypassed in unit tests via mock injection. Discovery API calls (`DescribeRouteServers`, `DescribeRouteServerEndpoints`, `DescribeSubnets`) are mocked alongside the existing peer and instance mocks.

**E2E tests** read CR manifests from a profile directory (`test/e2e/manifests/<profile>/`). AWS E2E tests require `spec.aws` to be set. The `rosa-bgp-poc` profile is provided as an example; a custom profile matching the actual ROSA deployment is needed for real testing. The test framework derives expected state from the operator's discovered `status.peerGroups` and cluster nodes — no topology is hardcoded in the test code.

| Component | How discovered |
|:---|:---|
| Region, Route Server IDs | From `CUDNBgpConfig` CR in the profile |
| Endpoints, neighbor IPs, remote ASN, AZs | Auto-discovered by the operator from Route Server IDs |
| Router nodes + AZs | Listed from cluster using CR's routerNodeSelector + `topology.kubernetes.io/zone` |
| AWS credentials | Via IRSA (operator's ServiceAccount assumes IAM role) |
| Infrastructure | Provisioned externally (e.g., [rosa-bgp Terraform](https://github.com/msemanrh/rosa-bgp)) |

---

## Unit Tests (Mocked EC2)

Test the AWS platform package in isolation using a mocked EC2 client interface. No AWS credentials or cluster required.

### Credential Verification

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-AWS-01 | Valid credentials (IRSA) | Mock STS returns success | Platform created successfully |
| UT-AWS-02 | STS verification failure | Mock STS returns auth error | CredentialError returned |

### Provider ID → Instance ID + AZ

| ID | Test Case | Input | Expected Result |
|:---|:---|:---|:---|
| UT-AWS-03 | Valid provider ID | `aws:///us-east-1a/i-0abc123` | instanceID=`i-0abc123`, az=`us-east-1a` |
| UT-AWS-04 | Invalid provider IDs (defensive test only) | `gce:///zone/instance`, `aws:///us-east-1a/`, `aws:////i-0abc123`, `aws://something` | Error for each |

### Route Server Endpoint Discovery

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-AWS-05 | Discover endpoints for single Route Server | 1 RS with 6 endpoints across 3 subnets | Endpoints grouped by AZ (derived from DescribeSubnets), ENI addresses and remote ASN returned |
| UT-AWS-06 | Discover endpoints for multiple Route Servers | 2 RS IDs, each with endpoints across 3 subnets | All endpoints from both RSs merged into per-AZ map |
| UT-AWS-07 | Route Server not found | Invalid RS ID, DescribeRouteServers returns empty | Error returned with invalid Route Server ID |
| UT-AWS-08 | API failure | DescribeRouteServers returns error | Error propagated |

### Route Server Peer Reconciliation

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-AWS-09 | Multi-AZ create | Empty peer list, nodes across 3 AZs, endpoints from discovery | Peers created only on correct AZ endpoints, correct ASN, tagged `managed-by: bgp-cloud-connector/<infrastructureName>` |
| UT-AWS-10 | Adopt pre-existing untagged peer | Untagged peer exists with same IP as a desired node | No create call; peer adopted via CreateTags with `managed-by: bgp-cloud-connector/<infrastructureName>` |
| UT-AWS-11 | Delete stale peer | 1 managed peer, no desired nodes | Peer deleted, no create calls |
| UT-AWS-12 | BFD liveness detection | livenessDetection=bfd | CreateRouteServerPeer includes BFD peer liveness mode |
| UT-AWS-13 | Cleanup deletes all managed peers | 6 managed peers across 3 AZs | All 6 deleted |

### SourceDestCheck

| ID | Test Case | Setup | Expected Result |
|:---|:---|:---|:---|
| UT-AWS-14 | Disable on node with check enabled | SourceDestCheck=true on primary ENI | ModifyNetworkInterfaceAttribute called with false |
| UT-AWS-15 | No-op when already disabled | SourceDestCheck=false on primary ENI | No modify call |
| UT-AWS-16 | No primary ENI found (defensive test only) | Instance with no device index 0 | Error returned |

---

## E2E Tests (ROSA HCP Cluster)

Full end-to-end tests running the operator on a ROSA HCP cluster with VPC Route Server infrastructure. Validates the complete reconciliation loop from CR creation through AWS resource state.

**Prerequisites:** ROSA HCP cluster provisioned by rosa-bgp Terraform, IRSA IAM role configured for the operator's ServiceAccount.

### Initial Deployment

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-AWS-01 | Full stack reconcile | Deploy operator, create labeled namespace, apply CUDNBgpConfig and CUDNBgpRouting CRs | Operator Running; config phase=Ready; `status.peerGroups` populated with the discovered plan, one group per AZ; FRRConfigurations created per discovered AZ with discovered neighbor addresses; Route Server peers exist per AZ; SourceDestCheck=false on router nodes; routing phase=Ready with CUDN + RouteAdvertisements; FRR pods show established BGP sessions |

### Node Lifecycle

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-AWS-02 | Node-to-peer consistency | Record current router nodes and managed peers | Every router node IP has a corresponding Route Server peer; SourceDestCheck=false on all router nodes |

### Self-Healing and Drift Recovery

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-AWS-03 | Route Server peer manually deleted | Delete a managed peer via AWS CLI | Operator recreates it within 5 minutes |
| E2E-AWS-04 | SourceDestCheck manually re-enabled | Enable SourceDestCheck on a router node ENI | Operator disables it within 5 minutes |

### Deletion and Cleanup

| ID | Test Case | Action | Verification |
|:---|:---|:---|:---|
| E2E-AWS-05 | Full cleanup lifecycle | Delete config (blocked by routing CR → requeues) → delete routing CR → delete config CR | AWS peers deleted, FRRConfigurations deleted, finalizer removed; Network patch intentionally left in place |

---

## How to Run

### Unit Tests

No credentials or cluster required.

```bash
# All unit tests (controllers + AWS platform package)
make test && make test-aws

# AWS platform package only
make test-aws
```

### E2E Tests

Full operator lifecycle on a ROSA HCP cluster with VPC Route Server infrastructure.

```bash
# Prerequisites:
# - Infrastructure provisioned (Terraform or equivalent)
# - oc login to ROSA HCP cluster
# - Operator deployed to the cluster
# - IRSA IAM role configured for operator ServiceAccount
# - AWS credentials configured on the test runner

# Profile is mandatory — specifies which CRs to apply (must have spec.aws)
make test-e2e-aws rosa-bgp-poc 
```

Profiles are directories under `test/e2e/manifests/` containing `cudnbgpconfig.yaml` and `cudnbgprouting.yaml`. To test your own ROSA cluster, create a profile directory with your CRs, configure IRSA for the operator's ServiceAccount, and run `make test-e2e-aws <profile-name>`.

#### AWS credentials for the test runner

The operator authenticates to AWS via IRSA (inside the cluster). The test binary runs outside the cluster and makes its own AWS API calls to verify and mutate AWS resources (Route Server peers, SourceDestCheck). It uses the [AWS default credential chain](https://docs.aws.amazon.com/sdkref/latest/guide/standardized-credentials.html) and needs the same EC2/STS permissions as the operator:

```bash
export AWS_ACCESS_KEY_ID=<key>
export AWS_SECRET_ACCESS_KEY=<secret>
export AWS_REGION=us-east-1       # must match spec.aws.region in the profile
```

Alternatively, configure credentials via `~/.aws/credentials` or `aws sso login`.
