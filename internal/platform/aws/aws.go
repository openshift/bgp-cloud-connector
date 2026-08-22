package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

type ec2API interface {
	DescribeRouteServers(ctx context.Context, input *ec2.DescribeRouteServersInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRouteServersOutput, error)
	DescribeRouteServerEndpoints(ctx context.Context, input *ec2.DescribeRouteServerEndpointsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRouteServerEndpointsOutput, error)
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeRouteServerPeers(ctx context.Context, input *ec2.DescribeRouteServerPeersInput, optFns ...func(*ec2.Options)) (*ec2.DescribeRouteServerPeersOutput, error)
	CreateRouteServerPeer(ctx context.Context, input *ec2.CreateRouteServerPeerInput, optFns ...func(*ec2.Options)) (*ec2.CreateRouteServerPeerOutput, error)
	DeleteRouteServerPeer(ctx context.Context, input *ec2.DeleteRouteServerPeerInput, optFns ...func(*ec2.Options)) (*ec2.DeleteRouteServerPeerOutput, error)
	CreateTags(ctx context.Context, input *ec2.CreateTagsInput, optFns ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DescribeInstances(ctx context.Context, input *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
}

type stsAPI interface {
	GetCallerIdentity(ctx context.Context, input *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

type Platform struct {
	ec2Client         ec2API
	region            string
	routeServerIDs    []string
	endpointsByAZ     map[string][]string // populated by DiscoverEndpoints
	localASN          int64
	livenessDetection string
	clusterID         string
}

type Config struct {
	Region            string
	RouteServerIDs    []string
	LocalASN          int64
	LivenessDetection string
	ClusterID         string
}

func New(ctx context.Context, cfg Config) (*Platform, error) {
	return newPlatform(ctx, cfg, nil, nil)
}

func newPlatform(ctx context.Context, cfg Config, ec2Override ec2API, stsOverride stsAPI) (*Platform, error) {
	var ec2Client ec2API
	var stsClient stsAPI

	if ec2Override != nil && stsOverride != nil {
		ec2Client = ec2Override
		stsClient = stsOverride
	} else {
		awsCfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("loading AWS config: %w", err)
		}
		ec2Client = ec2.NewFromConfig(awsCfg)
		stsClient = sts.NewFromConfig(awsCfg)
	}

	if _, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("AWS credential verification failed (sts:GetCallerIdentity): %v", err)}
	}

	return &Platform{
		ec2Client:         ec2Client,
		region:            cfg.Region,
		routeServerIDs:    cfg.RouteServerIDs,
		localASN:          cfg.LocalASN,
		livenessDetection: cfg.LivenessDetection,
		clusterID:         cfg.ClusterID,
	}, nil
}

// RouteServerNotFoundError is returned when a route server ID in spec.aws.routeServerIDs
// does not exist in AWS. This is a terminal condition — the user must correct the spec.
type RouteServerNotFoundError struct {
	ID string
}

func (e *RouteServerNotFoundError) Error() string {
	return fmt.Sprintf("route server %q not found in AWS; verify spec.aws.routeServerIDs", e.ID)
}

func (p *Platform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	if err := p.reconcileRouteServerPeers(ctx, nodes); err != nil {
		return fmt.Errorf("reconciling route server peers: %w", err)
	}
	logger.Info("route server peers reconciled", "nodeCount", len(nodes))

	if err := p.disableSourceDestCheck(ctx, nodes); err != nil {
		return fmt.Errorf("disabling source/dest check: %w", err)
	}
	logger.Info("source/dest check verified", "nodeCount", len(nodes))

	return nil
}

func (p *Platform) Cleanup(ctx context.Context) error {
	return p.deleteAllManagedPeers(ctx)
}

// ParseProviderID extracts the instance ID from a Kubernetes Node's spec.providerID.
// Format: aws:///ZONE/INSTANCE-ID
func ParseProviderID(providerID string) (instanceID string, az string, err error) {
	if !strings.HasPrefix(providerID, "aws://") {
		return "", "", fmt.Errorf("not an AWS provider ID: %s", providerID)
	}
	parts := strings.Split(strings.TrimPrefix(providerID, "aws:///"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed AWS provider ID: %s", providerID)
	}
	return parts[1], parts[0], nil
}

func managedByTagKey() string {
	return "managed-by"
}

func (p *Platform) peerTagValue() string {
	return "cudn-bgp-routing-operator/" + p.clusterID
}

func (p *Platform) peerTags() []ec2types.Tag {
	return []ec2types.Tag{
		{Key: aws.String(managedByTagKey()), Value: aws.String(p.peerTagValue())},
	}
}
