package aws

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func (p *Platform) disableSourceDestCheck(ctx context.Context, nodes []platform.RouterNode) ([]platform.RouterNode, []platform.RouterNode, error) {
	logger := log.FromContext(ctx)
	forwardingNodes := make([]platform.RouterNode, 0, len(nodes))
	unknownNodes := make([]platform.RouterNode, 0, len(nodes))
	var errs []error

	// Unknown nodes could not be inspected, so their existing peers must be
	// preserved. Once SourceDestCheck is observed enabled, a failed update is a
	// known unsafe state and the node belongs in neither result.
	for _, node := range nodes {
		instanceID, _, err := ParseProviderID(node.ProviderID)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			unknownNodes = append(unknownNodes, node)
			continue
		}

		eniID, srcDstEnabled, err := p.getPrimaryENI(ctx, instanceID)
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s (instance %s): %w", node.Name, instanceID, err))
			unknownNodes = append(unknownNodes, node)
			continue
		}

		if srcDstEnabled {
			logger.Info("disabling SourceDestCheck", "node", node.Name, "instance", instanceID, "eni", eniID)
			if err := p.setSourceDestCheck(ctx, eniID, false); err != nil {
				errs = append(errs, fmt.Errorf("node %s (eni %s): %w", node.Name, eniID, err))
				continue
			}
		}
		forwardingNodes = append(forwardingNodes, node)
	}
	return forwardingNodes, unknownNodes, errors.Join(errs...)
}

func (p *Platform) getPrimaryENI(ctx context.Context, instanceID string) (eniID string, srcDstCheckEnabled bool, err error) {
	input := &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}
	output, err := p.ec2Client.DescribeInstances(ctx, input)
	if err != nil {
		platform.RecordCloudAPIError(platform.PlatformAWS, platform.OpNodeForwarding)
		return "", false, err
	}

	for _, res := range output.Reservations {
		for _, inst := range res.Instances {
			for _, iface := range inst.NetworkInterfaces {
				if iface.Attachment != nil && aws.ToInt32(iface.Attachment.DeviceIndex) == 0 {
					enabled := iface.SourceDestCheck != nil && *iface.SourceDestCheck
					return aws.ToString(iface.NetworkInterfaceId), enabled, nil
				}
			}
		}
	}
	// Not counted: the call worked, the instance just has no primary ENI in
	// what it returned, which is not a failure of the AWS API.
	return "", false, fmt.Errorf("primary ENI not found for instance %s", instanceID)
}

func (p *Platform) setSourceDestCheck(ctx context.Context, eniID string, enabled bool) error {
	input := &ec2.ModifyNetworkInterfaceAttributeInput{
		NetworkInterfaceId: aws.String(eniID),
		SourceDestCheck:    &ec2types.AttributeBooleanValue{Value: aws.Bool(enabled)},
	}
	_, err := p.ec2Client.ModifyNetworkInterfaceAttribute(ctx, input)
	if err != nil {
		platform.RecordCloudAPIError(platform.PlatformAWS, platform.OpNodeForwarding)
	}
	return err
}
