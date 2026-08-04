package aws

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	logger := log.FromContext(ctx)

	result := &platform.DiscoveryResult{
		NeighborsByAZ: make(map[string][]platform.DiscoveredNeighbor),
		EndpointsByAZ: make(map[string][]string),
	}

	for _, rsID := range p.routeServerIDs {
		rs, err := p.describeRouteServer(ctx, rsID)
		if err != nil {
			return nil, fmt.Errorf("describing route server %s: %w", rsID, err)
		}

		remoteASN := aws.ToInt64(rs.AmazonSideAsn)
		logger.Info("discovered route server", "routeServerID", rsID, "remoteASN", remoteASN)

		endpoints, err := p.describeRouteServerEndpoints(ctx, rsID)
		if err != nil {
			return nil, fmt.Errorf("listing endpoints for route server %s: %w", rsID, err)
		}

		subnetIDs := make([]string, 0, len(endpoints))
		for _, ep := range endpoints {
			if ep.SubnetId != nil {
				subnetIDs = append(subnetIDs, *ep.SubnetId)
			}
		}
		subnetAZMap, err := p.resolveSubnetAZs(ctx, subnetIDs)
		if err != nil {
			return nil, fmt.Errorf("resolving subnet AZs for route server %s: %w", rsID, err)
		}

		discoveredRS := platform.DiscoveredRouteServer{
			RouteServerID: rsID,
			RemoteASN:     remoteASN,
		}

		for _, ep := range endpoints {
			epID := aws.ToString(ep.RouteServerEndpointId)
			address := aws.ToString(ep.EniAddress)
			subnetID := aws.ToString(ep.SubnetId)
			az := subnetAZMap[subnetID]

			logger.Info("discovered endpoint", "endpointID", epID, "az", az, "address", address)

			discoveredRS.Endpoints = append(discoveredRS.Endpoints, platform.DiscoveredEndpoint{
				EndpointID: epID,
				AZ:         az,
				Address:    address,
			})

			result.NeighborsByAZ[az] = append(result.NeighborsByAZ[az], platform.DiscoveredNeighbor{
				Address: address,
				ASN:     remoteASN,
			})
			result.EndpointsByAZ[az] = append(result.EndpointsByAZ[az], epID)
		}

		result.RouteServers = append(result.RouteServers, discoveredRS)
	}

	p.endpointsByAZ = result.EndpointsByAZ
	return result, nil
}

func (p *Platform) describeRouteServer(ctx context.Context, routeServerID string) (*ec2types.RouteServer, error) {
	output, err := p.ec2Client.DescribeRouteServers(ctx, &ec2.DescribeRouteServersInput{
		RouteServerIds: []string{routeServerID},
	})
	if err != nil {
		return nil, err
	}
	if len(output.RouteServers) == 0 {
		return nil, fmt.Errorf("route server %s not found", routeServerID)
	}
	return &output.RouteServers[0], nil
}

func (p *Platform) describeRouteServerEndpoints(ctx context.Context, routeServerID string) ([]ec2types.RouteServerEndpoint, error) {
	var filtered []ec2types.RouteServerEndpoint
	var nextToken *string
	for {
		output, err := p.ec2Client.DescribeRouteServerEndpoints(ctx, &ec2.DescribeRouteServerEndpointsInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		for _, ep := range output.RouteServerEndpoints {
			if aws.ToString(ep.RouteServerId) == routeServerID {
				filtered = append(filtered, ep)
			}
		}
		if aws.ToString(output.NextToken) == "" {
			break
		}
		nextToken = output.NextToken
	}
	return filtered, nil
}

func (p *Platform) resolveSubnetAZs(ctx context.Context, subnetIDs []string) (map[string]string, error) {
	if len(subnetIDs) == 0 {
		return nil, nil
	}
	output, err := p.ec2Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: subnetIDs,
	})
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(output.Subnets))
	for _, s := range output.Subnets {
		result[aws.ToString(s.SubnetId)] = aws.ToString(s.AvailabilityZone)
	}
	return result, nil
}
