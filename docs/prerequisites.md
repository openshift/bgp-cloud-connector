# Prerequisites

Follow the set of prerequisites that corresponds to the cloud provider you are using.

## AWS

### OpenShift Cluster

* OpenShift 4.22 or newer
* either ROSA HCP or self-managed OpenShift
* the cluster must be an STS cluster
* the OVN-Kubernetes CNI

### IAM Role

The operator will assume an IAM role in order to interact with networking resources, create Route Server Peers, and to disable source/destination checks on worker node ENIs.

Create an IAM role with a trust policy for the operator's ServiceAccount:

```bash
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
OIDC_PROVIDER="<my-oidc-provider-name>"
# On ROSA, use
#OIDC_PROVIDER=$(rosa describe cluster -c <cluster-name> -o json | jq -r '.aws.sts.oidc_endpoint_url' | sed 's|https://||')

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

Step 2 — Attach the required permissions policy:

```bash
aws iam put-role-policy --role-name cudn-bgp-operator \
  --policy-name cudn-bgp-operator-policy \
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

### VPC

The operator is desgined to work on the same VPC your OpenShift cluster is installed in.

The address ranges for routed CUDNs should not overlap with any address ranges of the VPC.

The VPC, subnets, and cluster must all be in the same AWS account.

### Route Server

  1. [Create a Route Server](https://docs.aws.amazon.com/vpc/latest/userguide/route-server-tutorial-create.html) in the same AWS account that owns the VPC.
      1. Choose an **Amazon Side ASN** and note it for when you configure the operator.
      1. Set **Persist routes** to **Disable**. A worker node is only a valid next-hop for a route while it has an active BGP session.
      1. After creation, note the Route Server's id (starting with `rs-`) for when you configure the operator. 
  1. [Associate the Route Server](https://docs.aws.amazon.com/vpc/latest/userguide/route-server-tutorial-associate.html) with the cluster's VPC.
  1. [Create Route Server Endpoints](https://docs.aws.amazon.com/vpc/latest/userguide/route-server-tutorial-create-endpoints.html), two per subnet. These should be in each subnet your worker nodes are in. For a standard highly-available 3-AZ cluster, this means a total of 6 Route Server Endpoints. Note the IP address of each endpoint for when you configure the operator.

  The operator, once configured, will create and manage the set of Route Server Peers


