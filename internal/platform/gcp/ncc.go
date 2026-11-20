package gcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
)

// NewNCCClient builds an NCCClient from the credential the caller
// resolved, falling back the same way NewComputeClient does.
func NewNCCClient(ctx context.Context, project, region string, opts ...option.ClientOption) (NCCClient, error) {
	if len(opts) == 0 {
		opts = []option.ClientOption{option.WithScopes(networkconnectivity.CloudPlatformScope)}
	}
	svc, err := networkconnectivity.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &nccClient{
		svc:     svc,
		project: project,
		region:  region,
	}, nil
}

type nccClient struct {
	svc     *networkconnectivity.Service
	project string
	region  string
}

func (n *nccClient) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", n.project, n.region)
}

func (n *nccClient) spokeName(spokeID string) string {
	return fmt.Sprintf("%s/spokes/%s", n.parent(), spokeID)
}

func hubPath(project, hubName string) string {
	if strings.HasPrefix(hubName, "projects/") {
		return hubName
	}
	return fmt.Sprintf("projects/%s/locations/global/hubs/%s", project, hubName)
}

func (n *nccClient) ReconcileSpoke(ctx context.Context, spokeName, hubName string, nodes []RouterNode, siteToSite bool) (bool, error) {
	name := n.spokeName(spokeName)
	hub := hubPath(n.project, hubName)

	spoke, err := n.svc.Projects.Locations.Spokes.Get(name).Context(ctx).Do()
	if err != nil {
		if ge, ok := err.(*googleapi.Error); ok && ge.Code == 404 {
			return n.createSpoke(ctx, spokeName, hub, nodes, siteToSite)
		}
		return false, err
	}

	// GCP fixes site-to-site data transfer when a spoke is created and
	// refuses any patch whose mask names it, even with the value
	// unchanged. So the mask names only the instances, and a spec that
	// asks for a different setting is reported rather than patched.
	current := spoke.LinkedRouterApplianceInstances != nil &&
		spoke.LinkedRouterApplianceInstances.SiteToSiteDataTransfer

	changed := false
	if !spokeMatches(spoke, nodes) {
		spoke.LinkedRouterApplianceInstances = &networkconnectivity.LinkedRouterApplianceInstances{
			SiteToSiteDataTransfer: current,
			Instances:              applianceInstancesFromNodes(nodes),
		}
		op, err := n.svc.Projects.Locations.Spokes.Patch(name, spoke).
			UpdateMask("linkedRouterApplianceInstances.instances").
			Context(ctx).
			Do()
		if err != nil {
			return false, err
		}
		if err := n.waitLRO(ctx, op); err != nil {
			return false, err
		}
		changed = true
	}

	if current != siteToSite {
		return changed, fmt.Errorf("NCC spoke %s has siteToSiteDataTransfer %t and the spec asks for %t: %w",
			spokeName, current, siteToSite, errSiteToSiteFixed)
	}
	return changed, nil
}

// errSiteToSiteFixed marks a spoke whose site-to-site data transfer
// differs from the spec. It is the one ReconcileSpoke error that leaves
// the spoke otherwise reconciled, so callers carry on past it.
var errSiteToSiteFixed = errors.New("GCP accepts that setting only when the spoke is created, " +
	"so delete the BGPCloudConfiguration and create it again to change it")

// spokeMatches reports whether the spoke's instances are already the ones
// wanted, so that a pass with nothing to do sends no patch.
//
// Both the instances and the address each is reached on are compared:
// comparing the instances alone let a node keeping its instance while
// changing address read as nothing to do. Site-to-site data transfer is
// not compared here, since no patch can change it; ReconcileSpoke reports
// a mismatch instead.
func spokeMatches(spoke *networkconnectivity.Spoke, nodes []RouterNode) bool {
	linked := spoke.LinkedRouterApplianceInstances
	if linked == nil {
		return len(nodes) == 0
	}

	current := make(map[string]string, len(linked.Instances))
	for _, inst := range linked.Instances {
		if inst != nil && inst.VirtualMachine != "" {
			current[inst.VirtualMachine] = inst.IpAddress
		}
	}
	desired := make(map[string]string, len(nodes))
	for _, node := range nodes {
		desired[node.SelfLink] = node.IPAddress
	}
	return maps.Equal(current, desired)
}

func (n *nccClient) createSpoke(ctx context.Context, spokeID, hub string, nodes []RouterNode, siteToSite bool) (bool, error) {
	spoke := &networkconnectivity.Spoke{
		Hub:         hub,
		Description: "Router appliance spoke for OSD BGP routing (managed by controller)",
		LinkedRouterApplianceInstances: &networkconnectivity.LinkedRouterApplianceInstances{
			SiteToSiteDataTransfer: siteToSite,
			Instances:              applianceInstancesFromNodes(nodes),
		},
	}
	op, err := n.svc.Projects.Locations.Spokes.Create(n.parent(), spoke).
		SpokeId(spokeID).
		Context(ctx).
		Do()
	if err != nil {
		return false, err
	}
	if err := n.waitLRO(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

func applianceInstancesFromNodes(nodes []RouterNode) []*networkconnectivity.RouterApplianceInstance {
	sorted := append([]RouterNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]*networkconnectivity.RouterApplianceInstance, 0, len(sorted))
	for i := range sorted {
		out = append(out, &networkconnectivity.RouterApplianceInstance{
			VirtualMachine: sorted[i].SelfLink,
			IpAddress:      sorted[i].IPAddress,
		})
	}
	return out
}

func (n *nccClient) waitLRO(ctx context.Context, op *networkconnectivity.GoogleLongrunningOperation) error {
	if op == nil || op.Name == "" {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		cur, err := n.svc.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if cur.Done {
			if cur.Error != nil {
				return fmt.Errorf("operation failed: code=%d message=%s", cur.Error.Code, cur.Error.Message)
			}
			return nil
		}
	}
}

func (n *nccClient) DeleteSpoke(ctx context.Context, spokeName string) (bool, error) {
	name := n.spokeName(spokeName)
	op, err := n.svc.Projects.Locations.Spokes.Delete(name).Context(ctx).Do()
	if err != nil {
		if ge, ok := err.(*googleapi.Error); ok && ge.Code == 404 {
			return false, nil
		}
		return false, err
	}
	if err := n.waitLRO(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

func (n *nccClient) ListSpokesByPrefix(ctx context.Context, hubName, prefix string) ([]string, error) {
	wantHub := hubPath(n.project, hubName)
	prefixDash := prefix + "-"

	var ids []string
	pageToken := ""
	for {
		call := n.svc.Projects.Locations.Spokes.List(n.parent()).Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, s := range resp.Spokes {
			if s == nil || s.Hub != wantHub {
				continue
			}
			spokeID := s.Name[strings.LastIndex(s.Name, "/")+1:]
			if strings.HasPrefix(spokeID, prefixDash) {
				suffix := spokeID[len(prefixDash):]
				if _, err := strconv.Atoi(suffix); err == nil {
					ids = append(ids, spokeID)
				}
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	sort.Strings(ids)
	return ids, nil
}
