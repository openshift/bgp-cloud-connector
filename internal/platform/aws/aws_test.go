package aws

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// The peer these tests prune: present in EC2, absent from the plan.
const stalePeerID = "peer-stale"

// --- Mocks ---

type mockSTS struct {
	err error
}

func (m *mockSTS) GetCallerIdentity(_ context.Context, _ *sts.GetCallerIdentityInput, _ ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &sts.GetCallerIdentityOutput{}, nil
}

type mockEC2 struct {
	describeRSFunc    func(*ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error)
	describeRSEFunc   func(*ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error)
	describeSubFunc   func(*ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error)
	describePeersFunc func(*ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error)
	createPeerFunc    func(*ec2.CreateRouteServerPeerInput) (*ec2.CreateRouteServerPeerOutput, error)
	deletePeerFunc    func(*ec2.DeleteRouteServerPeerInput) (*ec2.DeleteRouteServerPeerOutput, error)
	createTagsFunc    func(*ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error)
	describeInstFunc  func(*ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error)
	modifyENIFunc     func(*ec2.ModifyNetworkInterfaceAttributeInput) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)

	createPeerCalls []*ec2.CreateRouteServerPeerInput
	deletePeerCalls []*ec2.DeleteRouteServerPeerInput
	createTagsCalls []*ec2.CreateTagsInput
	modifyENICalls  []*ec2.ModifyNetworkInterfaceAttributeInput
}

func (m *mockEC2) DescribeRouteServers(_ context.Context, input *ec2.DescribeRouteServersInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteServersOutput, error) {
	if m.describeRSFunc != nil {
		return m.describeRSFunc(input)
	}
	return &ec2.DescribeRouteServersOutput{}, nil
}

func (m *mockEC2) DescribeRouteServerEndpoints(_ context.Context, input *ec2.DescribeRouteServerEndpointsInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteServerEndpointsOutput, error) {
	if m.describeRSEFunc != nil {
		return m.describeRSEFunc(input)
	}
	return &ec2.DescribeRouteServerEndpointsOutput{}, nil
}

func (m *mockEC2) DescribeSubnets(_ context.Context, input *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if m.describeSubFunc != nil {
		return m.describeSubFunc(input)
	}
	return &ec2.DescribeSubnetsOutput{}, nil
}

func (m *mockEC2) DescribeRouteServerPeers(_ context.Context, input *ec2.DescribeRouteServerPeersInput, _ ...func(*ec2.Options)) (*ec2.DescribeRouteServerPeersOutput, error) {
	if m.describePeersFunc != nil {
		return m.describePeersFunc(input)
	}
	return &ec2.DescribeRouteServerPeersOutput{}, nil
}

func (m *mockEC2) CreateRouteServerPeer(_ context.Context, input *ec2.CreateRouteServerPeerInput, _ ...func(*ec2.Options)) (*ec2.CreateRouteServerPeerOutput, error) {
	m.createPeerCalls = append(m.createPeerCalls, input)
	if m.createPeerFunc != nil {
		return m.createPeerFunc(input)
	}
	return &ec2.CreateRouteServerPeerOutput{}, nil
}

func (m *mockEC2) DeleteRouteServerPeer(_ context.Context, input *ec2.DeleteRouteServerPeerInput, _ ...func(*ec2.Options)) (*ec2.DeleteRouteServerPeerOutput, error) {
	m.deletePeerCalls = append(m.deletePeerCalls, input)
	if m.deletePeerFunc != nil {
		return m.deletePeerFunc(input)
	}
	return &ec2.DeleteRouteServerPeerOutput{}, nil
}

func (m *mockEC2) CreateTags(_ context.Context, input *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	m.createTagsCalls = append(m.createTagsCalls, input)
	if m.createTagsFunc != nil {
		return m.createTagsFunc(input)
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (m *mockEC2) DescribeInstances(_ context.Context, input *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if m.describeInstFunc != nil {
		return m.describeInstFunc(input)
	}
	return &ec2.DescribeInstancesOutput{}, nil
}

func (m *mockEC2) ModifyNetworkInterfaceAttribute(_ context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifyNetworkInterfaceAttributeOutput, error) {
	m.modifyENICalls = append(m.modifyENICalls, input)
	if m.modifyENIFunc != nil {
		return m.modifyENIFunc(input)
	}
	return &ec2.ModifyNetworkInterfaceAttributeOutput{}, nil
}

// --- UT-AWS-01 to UT-AWS-02: Credential Verification ---

func TestNew_ValidCredentials(t *testing.T) {
	p, err := newPlatform(context.Background(), Config{
		Region:         "us-east-1",
		RouteServerIDs: []string{"rs-1"},
		LocalASN:       65001,
	}, &mockEC2{}, &mockSTS{})

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if p == nil {
		t.Fatal("expected platform to be non-nil")
	}
}

func TestNew_STSVerificationFailure(t *testing.T) {
	_, err := newPlatform(context.Background(), Config{
		Region: "us-east-1",
	}, &mockEC2{}, &mockSTS{err: errors.New("InvalidClientTokenId")})

	var credErr *platform.CredentialError
	if !errors.As(err, &credErr) {
		t.Fatalf("expected CredentialError, got: %v", err)
	}
}

// --- UT-AWS-03 to UT-AWS-04: Provider ID Parsing ---

func TestParseProviderID_Valid(t *testing.T) {
	instanceID, az, err := ParseProviderID("aws:///us-east-1a/i-0abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if instanceID != "i-0abc123" {
		t.Errorf("instanceID = %q, want %q", instanceID, "i-0abc123")
	}
	if az != "us-east-1a" {
		t.Errorf("az = %q, want %q", az, "us-east-1a")
	}
}

func TestParseProviderID_Invalid(t *testing.T) {
	cases := []string{
		"gce:///zone/instance",
		"aws:///us-east-1a/",
		"aws:////i-0abc123",
		"aws://something",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			_, _, err := ParseProviderID(input)
			if err == nil {
				t.Errorf("expected error for %q", input)
			}
		})
	}
}

// --- UT-AWS-09 to UT-AWS-13: Route Server Peer Reconciliation ---

func newTestPlatform(mock *mockEC2) *Platform {
	return &Platform{
		ec2Client:      mock,
		region:         "us-east-1",
		routeServerIDs: []string{"rs-1"},
		endpointsByAZ: map[string][]string{
			"us-east-1a": {"ep-a1", "ep-a2"},
			"us-east-1b": {"ep-b1", "ep-b2"},
			"us-east-1c": {"ep-c1", "ep-c2"},
		},
		localASN:          65001,
		livenessDetection: "",
		clusterID:         "test-cluster",
	}
}

func TestReconcilePeers_MultiAZCreate(t *testing.T) {
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{}, nil
		},
	}
	p := newTestPlatform(mock)

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
		{Name: "node-b", PrivateIP: "10.0.2.10", Zone: "us-east-1b", ProviderID: "aws:///us-east-1b/i-b"},
		{Name: "node-c", PrivateIP: "10.0.3.10", Zone: "us-east-1c", ProviderID: "aws:///us-east-1c/i-c"},
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 AZs × 2 endpoints × 1 node = 6 create calls
	if len(mock.createPeerCalls) != 6 {
		t.Fatalf("expected 6 create calls, got %d", len(mock.createPeerCalls))
	}

	for _, call := range mock.createPeerCalls {
		if call.BgpOptions == nil || aws.ToInt64(call.BgpOptions.PeerAsn) != 65001 {
			t.Error("expected ASN 65001 in create call")
		}
		if len(call.TagSpecifications) == 0 {
			t.Error("expected tag specifications in create call")
		} else {
			found := false
			for _, tag := range call.TagSpecifications[0].Tags {
				if aws.ToString(tag.Key) == "managed-by" && aws.ToString(tag.Value) == "cudn-bgp-routing-operator/test-cluster" {
					found = true
				}
			}
			if !found {
				t.Error("expected managed-by tag in create call")
			}
		}
	}
}

func TestReconcilePeers_AdoptPreExistingUntagged(t *testing.T) {
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{PeerAddress: aws.String("10.0.1.10"), RouteServerPeerId: aws.String("peer-existing"), RouteServerEndpointId: aws.String("ep-a1")},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.createPeerCalls) != 0 {
		t.Errorf("expected no create calls, got %d", len(mock.createPeerCalls))
	}
	if len(mock.createTagsCalls) != 1 {
		t.Fatalf("expected 1 CreateTags call, got %d", len(mock.createTagsCalls))
	}
	if mock.createTagsCalls[0].Resources[0] != "peer-existing" {
		t.Errorf("expected tag on peer-existing, got %v", mock.createTagsCalls[0].Resources)
	}
}

func TestReconcilePeers_DeleteStalePeer(t *testing.T) {
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{
						PeerAddress:           aws.String("10.0.1.99"),
						RouteServerPeerId:     aws.String(stalePeerID),
						RouteServerEndpointId: aws.String("ep-a1"),
						Tags: []ec2types.Tag{
							{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")},
						},
					},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	// No desired nodes in this AZ — the managed peer is stale
	if err := p.reconcileRouteServerPeers(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.deletePeerCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mock.deletePeerCalls))
	}
	if aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId) != stalePeerID {
		t.Errorf("expected delete of peer-stale, got %s", aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId))
	}
	if len(mock.createPeerCalls) != 0 {
		t.Errorf("expected no create calls, got %d", len(mock.createPeerCalls))
	}
}

func TestReconcilePeers_BFDLivenessDetection(t *testing.T) {
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{}, nil
		},
	}
	p := &Platform{
		ec2Client:         mock,
		endpointsByAZ:     map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:          65001,
		livenessDetection: "bfd",
		clusterID:         "test-cluster",
	}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.createPeerCalls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(mock.createPeerCalls))
	}
	bgpOpts := mock.createPeerCalls[0].BgpOptions
	if bgpOpts == nil || bgpOpts.PeerLivenessDetection != ec2types.RouteServerPeerLivenessMode("bfd") {
		t.Error("expected BFD peer liveness mode in create call")
	}
}

func TestReconcilePeers_SkipsManagedPeerWithNilFields(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					// Managed (tagged) peer missing both PeerAddress and
					// RouteServerPeerId — the nil checks below must skip it;
					// removing either one panics on the dereference.
					{RouteServerEndpointId: aws.String("ep-a1"), Tags: managedTag},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}
	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The nil-field peer is invisible to the managed set, so the desired IP
	// still gets a fresh peer created for it.
	if len(mock.createPeerCalls) != 1 {
		t.Fatalf("expected 1 create call despite the nil-field managed peer, got %d", len(mock.createPeerCalls))
	}
}

func TestCleanup_DeletesAllManagedPeers(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{PeerAddress: aws.String("10.0.0.1"), RouteServerPeerId: aws.String("peer-ep-a1"), RouteServerEndpointId: aws.String("ep-a1"), Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.2"), RouteServerPeerId: aws.String("peer-ep-a2"), RouteServerEndpointId: aws.String("ep-a2"), Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.3"), RouteServerPeerId: aws.String("peer-ep-b1"), RouteServerEndpointId: aws.String("ep-b1"), Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.4"), RouteServerPeerId: aws.String("peer-ep-b2"), RouteServerEndpointId: aws.String("ep-b2"), Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.5"), RouteServerPeerId: aws.String("peer-ep-c1"), RouteServerEndpointId: aws.String("ep-c1"), Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.6"), RouteServerPeerId: aws.String("peer-ep-c2"), RouteServerEndpointId: aws.String("ep-c2"), Tags: managedTag},
				},
			}, nil
		},
	}
	p := newTestPlatform(mock)

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.deletePeerCalls) != 6 {
		t.Errorf("expected 6 delete calls (3 AZs × 2 endpoints), got %d", len(mock.deletePeerCalls))
	}
}

func TestCleanup_SkipsManagedPeerWithNilRouteServerPeerId(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					// Managed (tagged) peer missing RouteServerPeerId — the
					// nil check below must skip it; removing it panics on
					// the dereference.
					{PeerAddress: aws.String("10.0.0.1"), RouteServerEndpointId: aws.String("ep-a1"), Tags: managedTag},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		clusterID:     "test-cluster",
	}

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.deletePeerCalls) != 0 {
		t.Errorf("expected no delete calls for a peer with nil RouteServerPeerId, got %d", len(mock.deletePeerCalls))
	}
}

// --- UT-AWS-14 to UT-AWS-16: SourceDestCheck ---

func TestDisableSourceDestCheck_EnabledToDisabled(t *testing.T) {
	mock := &mockEC2{
		describeInstFunc: func(_ *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{
					Instances: []ec2types.Instance{{
						NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
							NetworkInterfaceId: aws.String("eni-primary"),
							SourceDestCheck:    aws.Bool(true),
							Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
						}},
					}},
				}},
			}, nil
		},
	}
	p := &Platform{ec2Client: mock}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}
	if err := p.disableSourceDestCheck(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.modifyENICalls) != 1 {
		t.Fatalf("expected 1 modify call, got %d", len(mock.modifyENICalls))
	}
	if aws.ToString(mock.modifyENICalls[0].NetworkInterfaceId) != "eni-primary" {
		t.Errorf("expected eni-primary, got %s", aws.ToString(mock.modifyENICalls[0].NetworkInterfaceId))
	}
	if aws.ToBool(mock.modifyENICalls[0].SourceDestCheck.Value) != false {
		t.Error("expected SourceDestCheck set to false")
	}
}

func TestDisableSourceDestCheck_AlreadyDisabled(t *testing.T) {
	mock := &mockEC2{
		describeInstFunc: func(_ *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{
					Instances: []ec2types.Instance{{
						NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
							NetworkInterfaceId: aws.String("eni-primary"),
							SourceDestCheck:    aws.Bool(false),
							Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
						}},
					}},
				}},
			}, nil
		},
	}
	p := &Platform{ec2Client: mock}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}
	if err := p.disableSourceDestCheck(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.modifyENICalls) != 0 {
		t.Errorf("expected no modify calls when already disabled, got %d", len(mock.modifyENICalls))
	}
}

func TestDisableSourceDestCheck_NoPrimaryENI(t *testing.T) {
	mock := &mockEC2{
		describeInstFunc: func(_ *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{
					Instances: []ec2types.Instance{{
						NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
							NetworkInterfaceId: aws.String("eni-secondary"),
							SourceDestCheck:    aws.Bool(true),
							Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(1)},
						}},
					}},
				}},
			}, nil
		},
	}
	p := &Platform{ec2Client: mock}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}
	err := p.disableSourceDestCheck(context.Background(), nodes)
	if err == nil {
		t.Fatal("expected error when no primary ENI found")
	}
}

// --- UT-AWS-05 to UT-AWS-08: Endpoint Discovery ---

func newDiscoveryMock() *mockEC2 {
	return &mockEC2{
		describeRSFunc: func(input *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			return &ec2.DescribeRouteServersOutput{
				RouteServers: []ec2types.RouteServer{
					{RouteServerId: aws.String(input.RouteServerIds[0]), AmazonSideAsn: aws.Int64(64512)},
				},
			}, nil
		},
		describeRSEFunc: func(_ *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-a1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.47")},
					{RouteServerEndpointId: aws.String("rse-a2"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.183")},
					{RouteServerEndpointId: aws.String("rse-b1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-b"), EniAddress: aws.String("10.0.2.91")},
					{RouteServerEndpointId: aws.String("rse-b2"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-b"), EniAddress: aws.String("10.0.2.145")},
					{RouteServerEndpointId: aws.String("rse-c1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-c"), EniAddress: aws.String("10.0.3.62")},
					{RouteServerEndpointId: aws.String("rse-c2"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-c"), EniAddress: aws.String("10.0.3.118")},
				},
			}, nil
		},
		describeSubFunc: func(_ *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			return &ec2.DescribeSubnetsOutput{
				Subnets: []ec2types.Subnet{
					{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("us-east-1a")},
					{SubnetId: aws.String("subnet-b"), AvailabilityZone: aws.String("us-east-1b")},
					{SubnetId: aws.String("subnet-c"), AvailabilityZone: aws.String("us-east-1c")},
				},
			}, nil
		},
	}
}

func TestDiscoverEndpoints_Success(t *testing.T) {
	mock := newDiscoveryMock()
	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One group per zone, in zone order, because the group's position names
	// the FRRConfiguration it becomes.
	if len(result.PeerGroups) != 3 {
		t.Fatalf("expected 3 peer groups, got %d", len(result.PeerGroups))
	}
	for i, az := range []string{"us-east-1a", "us-east-1b", "us-east-1c"} {
		g := result.PeerGroups[i]
		if g.Key != az {
			t.Errorf("group %d key = %q, want %q", i, g.Key, az)
		}
		if got := g.NodeSelector["topology.kubernetes.io/zone"]; got != az {
			t.Errorf("group %q selects zone %q; key and selector should agree", g.Key, got)
		}
		if len(g.Neighbors) != 2 {
			t.Errorf("expected 2 neighbours in %s, got %d", az, len(g.Neighbors))
		}
		for _, n := range g.Neighbors {
			if n.ASN != 64512 {
				t.Errorf("neighbour %s ASN = %d, want 64512", n.Address, n.ASN)
			}
		}
	}
	if len(p.endpointsByAZ) != 3 {
		t.Errorf("expected endpointsByAZ populated with 3 AZs, got %d", len(p.endpointsByAZ))
	}
}

func TestDiscoverEndpoints_RouteServerNotFound(t *testing.T) {
	mock := &mockEC2{
		describeRSFunc: func(_ *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			return &ec2.DescribeRouteServersOutput{}, nil
		},
	}
	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-nonexistent"},
	}

	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected error for non-existent route server")
	}
}

func TestDiscoverEndpoints_MultipleRouteServers(t *testing.T) {
	mock := &mockEC2{
		describeRSFunc: func(input *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			asn := int64(64512)
			if input.RouteServerIds[0] == "rs-2" {
				asn = 64513
			}
			return &ec2.DescribeRouteServersOutput{
				RouteServers: []ec2types.RouteServer{
					{RouteServerId: aws.String(input.RouteServerIds[0]), AmazonSideAsn: aws.Int64(asn)},
				},
			}, nil
		},
		describeRSEFunc: func(_ *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-1a"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.10")},
					{RouteServerEndpointId: aws.String("rse-2a"), RouteServerId: aws.String("rs-2"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.20")},
				},
			}, nil
		},
		describeSubFunc: func(_ *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			return &ec2.DescribeSubnetsOutput{
				Subnets: []ec2types.Subnet{
					{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("us-east-1a")},
				},
			}, nil
		},
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1", "rs-2"},
	}

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two route servers in one zone collapse into one group: a node peers
	// with every endpoint in its zone, whichever server they belong to.
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected 1 peer group, got %d", len(result.PeerGroups))
	}
	if got := result.PeerGroups[0].Neighbors; len(got) != 2 {
		t.Errorf("expected 2 neighbours in us-east-1a from 2 route servers, got %d", len(got))
	}
}

func TestDiscoverEndpoints_APIFailure(t *testing.T) {
	mock := &mockEC2{
		describeRSFunc: func(_ *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			return nil, errors.New("ec2 API failure")
		},
	}
	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}

	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected error on API failure")
	}
}

func TestDiscoverEndpoints_PaginatesEndpoints(t *testing.T) {
	calls := 0
	mock := newDiscoveryMock()
	mock.describeRSEFunc = func(input *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
		calls++
		if input.NextToken == nil {
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-a1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.47")},
					{RouteServerEndpointId: aws.String("rse-b1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-b"), EniAddress: aws.String("10.0.2.91")},
				},
				NextToken: aws.String("page-2"),
			}, nil
		}
		if aws.ToString(input.NextToken) != "page-2" {
			t.Fatalf("unexpected NextToken %q", aws.ToString(input.NextToken))
		}
		return &ec2.DescribeRouteServerEndpointsOutput{
			RouteServerEndpoints: []ec2types.RouteServerEndpoint{
				{RouteServerEndpointId: aws.String("rse-c1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-c"), EniAddress: aws.String("10.0.3.62")},
			},
		}, nil
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}
	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 DescribeRouteServerEndpoints calls, got %d", calls)
	}
	total := 0
	for _, g := range result.PeerGroups {
		total += len(g.Neighbors)
	}
	if total != 3 {
		t.Fatalf("expected 3 endpoints across pages, got %d", total)
	}
}

func TestDiscoverEndpoints_EmptyStringNextTokenStops(t *testing.T) {
	calls := 0
	mock := newDiscoveryMock()
	mock.describeRSEFunc = func(input *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
		calls++
		if input.NextToken != nil {
			t.Fatalf("expected no second page call, got NextToken %q", aws.ToString(input.NextToken))
		}
		return &ec2.DescribeRouteServerEndpointsOutput{
			RouteServerEndpoints: []ec2types.RouteServerEndpoint{
				{RouteServerEndpointId: aws.String("rse-a1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.47")},
			},
			NextToken: aws.String(""),
		}, nil
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}
	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call when NextToken is empty string, got %d", calls)
	}
	if len(result.PeerGroups) != 1 || len(result.PeerGroups[0].Neighbors) != 1 {
		t.Fatalf("expected 1 endpoint in 1 group, got %v", result.PeerGroups)
	}
}

func TestDiscoverEndpoints_Page2Error(t *testing.T) {
	mock := newDiscoveryMock()
	mock.describeRSEFunc = func(input *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
		if input.NextToken == nil {
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-a1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.47")},
				},
				NextToken: aws.String("page-2"),
			}, nil
		}
		return nil, errors.New("ec2 API failure on page 2")
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}
	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected error on page 2 failure")
	}
}

func TestDiscoverEndpoints_EmptySubnetAZ(t *testing.T) {
	mock := &mockEC2{
		describeRSFunc: func(input *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			return &ec2.DescribeRouteServersOutput{
				RouteServers: []ec2types.RouteServer{
					{RouteServerId: aws.String(input.RouteServerIds[0]), AmazonSideAsn: aws.Int64(64512)},
				},
			}, nil
		},
		describeRSEFunc: func(_ *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-a1"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.47")},
				},
			}, nil
		},
		describeSubFunc: func(_ *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			// DescribeSubnets errors out entirely if a requested subnet ID is
			// unknown to AWS, so the only way this map ends up incomplete for
			// a real request is a subnet that exists but reports no AZ.
			return &ec2.DescribeSubnetsOutput{Subnets: []ec2types.Subnet{
				{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("")},
			}}, nil
		},
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1"},
	}
	_, err := p.DiscoverEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected error when a subnet reports an empty availability zone")
	}
}

func TestDiscoverEndpoints_MultipleRouteServers_Paginates(t *testing.T) {
	calls := 0
	paginationRestarts := 0
	mock := &mockEC2{
		describeRSFunc: func(input *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			asn := int64(64512)
			if input.RouteServerIds[0] == "rs-2" {
				asn = 64513
			}
			return &ec2.DescribeRouteServersOutput{
				RouteServers: []ec2types.RouteServer{
					{RouteServerId: aws.String(input.RouteServerIds[0]), AmazonSideAsn: aws.Int64(asn)},
				},
			}, nil
		},
		describeRSEFunc: func(input *ec2.DescribeRouteServerEndpointsInput) (*ec2.DescribeRouteServerEndpointsOutput, error) {
			calls++
			if input.NextToken == nil {
				paginationRestarts++
				return &ec2.DescribeRouteServerEndpointsOutput{
					RouteServerEndpoints: []ec2types.RouteServerEndpoint{
						{RouteServerEndpointId: aws.String("rse-1a"), RouteServerId: aws.String("rs-1"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.10")},
					},
					NextToken: aws.String("page-2"),
				}, nil
			}
			if aws.ToString(input.NextToken) != "page-2" {
				t.Fatalf("unexpected NextToken %q", aws.ToString(input.NextToken))
			}
			return &ec2.DescribeRouteServerEndpointsOutput{
				RouteServerEndpoints: []ec2types.RouteServerEndpoint{
					{RouteServerEndpointId: aws.String("rse-2a"), RouteServerId: aws.String("rs-2"), SubnetId: aws.String("subnet-a"), EniAddress: aws.String("10.0.1.20")},
				},
			}, nil
		},
		describeSubFunc: func(_ *ec2.DescribeSubnetsInput) (*ec2.DescribeSubnetsOutput, error) {
			return &ec2.DescribeSubnetsOutput{
				Subnets: []ec2types.Subnet{
					{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("us-east-1a")},
				},
			}, nil
		},
	}

	p := &Platform{
		ec2Client:      mock,
		routeServerIDs: []string{"rs-1", "rs-2"},
	}
	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if paginationRestarts != 2 {
		t.Fatalf("expected 2 pagination restarts (one per RS), got %d", paginationRestarts)
	}
	if calls != 4 {
		t.Fatalf("expected 4 endpoint describe calls (2 RS × 2 pages), got %d", calls)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("expected 1 peer group, got %d", len(result.PeerGroups))
	}
	if got := result.PeerGroups[0].Neighbors; len(got) != 2 {
		t.Errorf("expected 2 neighbours in us-east-1a across both route servers, got %d", len(got))
	}
}

func TestListAllPeers_PaginatesPeers(t *testing.T) {
	const wantEndpoint = "rse-a1"
	calls := 0
	mock := &mockEC2{
		describePeersFunc: func(input *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			calls++
			if input.NextToken == nil {
				return &ec2.DescribeRouteServerPeersOutput{
					RouteServerPeers: []ec2types.RouteServerPeer{
						{RouteServerPeerId: aws.String("rsp-1"), RouteServerEndpointId: aws.String("rse-a1"), PeerAddress: aws.String("10.0.1.10")},
					},
					NextToken: aws.String("peers-2"),
				}, nil
			}
			if aws.ToString(input.NextToken) != "peers-2" {
				t.Fatalf("unexpected NextToken %q", aws.ToString(input.NextToken))
			}
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{RouteServerPeerId: aws.String("rsp-2"), RouteServerEndpointId: aws.String("rse-a1"), PeerAddress: aws.String("10.0.1.11")},
					{RouteServerPeerId: aws.String("rsp-other"), RouteServerEndpointId: aws.String("rse-other"), PeerAddress: aws.String("10.0.9.9")},
				},
			}, nil
		},
	}
	p := &Platform{ec2Client: mock}
	peers, err := p.listAllPeers(context.Background(), wantEndpoint)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 DescribeRouteServerPeers calls, got %d", calls)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers for %s across pages, got %d", wantEndpoint, len(peers))
	}
}

func TestListAllPeers_EmptyStringNextTokenStops(t *testing.T) {
	calls := 0
	mock := &mockEC2{
		describePeersFunc: func(input *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			calls++
			if input.NextToken != nil {
				t.Fatalf("expected no second page call, got NextToken %q", aws.ToString(input.NextToken))
			}
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{RouteServerPeerId: aws.String("rsp-1"), RouteServerEndpointId: aws.String("rse-a1"), PeerAddress: aws.String("10.0.1.10")},
				},
				NextToken: aws.String(""),
			}, nil
		},
	}
	p := &Platform{ec2Client: mock}
	peers, err := p.listAllPeers(context.Background(), "rse-a1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call when NextToken is empty string, got %d", calls)
	}
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
}

func TestListAllPeers_Page2Error(t *testing.T) {
	mock := &mockEC2{
		describePeersFunc: func(input *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			if input.NextToken == nil {
				return &ec2.DescribeRouteServerPeersOutput{
					RouteServerPeers: []ec2types.RouteServerPeer{
						{RouteServerPeerId: aws.String("rsp-1"), RouteServerEndpointId: aws.String("rse-a1"), PeerAddress: aws.String("10.0.1.10")},
					},
					NextToken: aws.String("peers-2"),
				}, nil
			}
			return nil, errors.New("ec2 API failure on page 2")
		},
	}
	p := &Platform{ec2Client: mock}
	_, err := p.listAllPeers(context.Background(), "rse-a1")
	if err == nil {
		t.Fatal("expected error on page 2 failure")
	}
}

func TestReconcilePeers_DeleteStalePeerOnPage2(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(input *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			if input.NextToken == nil {
				return &ec2.DescribeRouteServerPeersOutput{
					RouteServerPeers: []ec2types.RouteServerPeer{
						{PeerAddress: aws.String("10.0.1.1"), RouteServerPeerId: aws.String("peer-other"), RouteServerEndpointId: aws.String("ep-other")},
					},
					NextToken: aws.String("peers-2"),
				}, nil
			}
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{
						PeerAddress:           aws.String("10.0.1.99"),
						RouteServerPeerId:     aws.String(stalePeerID),
						RouteServerEndpointId: aws.String("ep-a1"),
						Tags:                  managedTag,
					},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.deletePeerCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mock.deletePeerCalls))
	}
	if aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId) != stalePeerID {
		t.Errorf("expected delete of peer-stale, got %s", aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId))
	}
}

func TestCleanup_DeletesManagedPeerOnPage2(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(input *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			if input.NextToken == nil {
				return &ec2.DescribeRouteServerPeersOutput{
					RouteServerPeers: []ec2types.RouteServerPeer{
						{PeerAddress: aws.String("10.0.0.9"), RouteServerPeerId: aws.String("peer-other"), RouteServerEndpointId: aws.String("ep-other")},
					},
					NextToken: aws.String("peers-2"),
				}, nil
			}
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{PeerAddress: aws.String("10.0.0.1"), RouteServerPeerId: aws.String("peer-ep-a1"), RouteServerEndpointId: aws.String("ep-a1"), Tags: managedTag},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client: mock,
		endpointsByAZ: map[string][]string{
			"us-east-1a": {"ep-a1"},
		},
		clusterID: "test-cluster",
	}

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.deletePeerCalls) != 1 {
		t.Fatalf("expected 1 delete call, got %d", len(mock.deletePeerCalls))
	}
	if aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId) != "peer-ep-a1" {
		t.Errorf("expected delete of peer-ep-a1, got %s", aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId))
	}
}

// managedPeersMock returns one managed peer per named endpoint, so a test can
// see which of them a reconcile pass would delete.
func managedPeersMock(peers map[string]string) *mockEC2 {
	return &mockEC2{
		// ReconcileNodes runs the source/dest check after the peers, so an
		// instance with a primary ENI has to exist for it to reach that far.
		describeInstFunc: func(_ *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return &ec2.DescribeInstancesOutput{
				Reservations: []ec2types.Reservation{{
					Instances: []ec2types.Instance{{
						NetworkInterfaces: []ec2types.InstanceNetworkInterface{{
							NetworkInterfaceId: aws.String("eni-primary"),
							SourceDestCheck:    aws.Bool(false),
							Attachment:         &ec2types.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int32(0)},
						}},
					}},
				}},
			}, nil
		},
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			var out []ec2types.RouteServerPeer
			for endpoint, addr := range peers {
				out = append(out, ec2types.RouteServerPeer{
					PeerAddress:           aws.String(addr),
					RouteServerPeerId:     aws.String("peer-" + endpoint),
					RouteServerEndpointId: aws.String(endpoint),
					Tags: []ec2types.Tag{
						{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")},
					},
				})
			}
			return &ec2.DescribeRouteServerPeersOutput{RouteServerPeers: out}, nil
		},
	}
}

// TestReconcileNodes_EmptyNodeListLeavesPeersAlone covers a router node
// selector that momentarily matches nothing, which happens whenever the label
// is being moved during a rollout.
//
// Reconciling an empty set the same way as a shrunk one makes every managed
// peer stale at once, so a transient gap in the node list tears down BGP for
// the whole cluster. Releasing the peers is what Cleanup is for, and it runs
// on deletion, where the intent is unambiguous.
func TestReconcileNodes_EmptyNodeListLeavesPeersAlone(t *testing.T) {
	mock := managedPeersMock(map[string]string{"ep-a1": "10.0.1.10"})
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	if err := p.ReconcileNodes(context.Background(), nil); err != nil {
		t.Fatalf("ReconcileNodes on an empty node list: %v", err)
	}

	if len(mock.deletePeerCalls) != 0 {
		t.Errorf("deleted %d peers, want 0: an empty node list must not release the peers", len(mock.deletePeerCalls))
	}
}

// TestReconcileNodes_ShrunkNodeListStillDeletes guards the early return from
// swallowing the case it must not: an AZ losing its last node while other AZs
// still have one has to reach EC2, or a removed node keeps its peer forever.
func TestReconcileNodes_ShrunkNodeListStillDeletes(t *testing.T) {
	mock := managedPeersMock(map[string]string{
		"ep-a1": "10.0.1.10",
		"ep-b1": "10.0.2.20",
	})
	p := &Platform{
		ec2Client: mock,
		endpointsByAZ: map[string][]string{
			"us-east-1a": {"ep-a1"},
			"us-east-1b": {"ep-b1"},
		},
		localASN:  65001,
		clusterID: "test-cluster",
	}

	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}

	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}

	if len(mock.deletePeerCalls) != 1 {
		t.Fatalf("deleted %d peers, want 1: us-east-1b has no node left", len(mock.deletePeerCalls))
	}
	if got := aws.ToString(mock.deletePeerCalls[0].RouteServerPeerId); got != "peer-ep-b1" {
		t.Errorf("deleted %s, want peer-ep-b1", got)
	}
}

// TestCleanup_SkipsPeersAlreadyGoing pins the state check that stops a delete
// wedging the finalizer.
//
// EC2 keeps returning a peer after it has been deleted, and refuses a delete on
// one that is already deleting or deleted with IncorrectState. Cleanup runs on
// the delete path, so an error there is not merely untidy: it propagates, the
// finalizer is never released, and the CUDNBgpConfig stays terminating for
// ever, retrying. Measured on a live cluster at over ten minutes, freed only
// by removing the finalizer by hand.
func TestCleanup_SkipsPeersAlreadyGoing(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{PeerAddress: aws.String("10.0.0.1"), RouteServerPeerId: aws.String("peer-live"), RouteServerEndpointId: aws.String("ep-a1"),
						State: ec2types.RouteServerPeerStateAvailable, Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.2"), RouteServerPeerId: aws.String("peer-deleting"), RouteServerEndpointId: aws.String("ep-a2"),
						State: ec2types.RouteServerPeerStateDeleting, Tags: managedTag},
					{PeerAddress: aws.String("10.0.0.3"), RouteServerPeerId: aws.String("peer-deleted"), RouteServerEndpointId: aws.String("ep-b1"),
						State: ec2types.RouteServerPeerStateDeleted, Tags: managedTag},
				},
			}, nil
		},
	}
	p := newTestPlatform(mock)

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup should not fail on peers already going: %v", err)
	}

	deleted := make([]string, 0, len(mock.deletePeerCalls))
	for _, call := range mock.deletePeerCalls {
		deleted = append(deleted, aws.ToString(call.RouteServerPeerId))
	}
	for _, id := range deleted {
		if id == "peer-deleting" || id == "peer-deleted" {
			t.Errorf("reissued a delete for %s, which EC2 refuses with IncorrectState", id)
		}
	}
	if len(deleted) != 1 || deleted[0] != "peer-live" {
		t.Errorf("expected only peer-live to be deleted, got %v", deleted)
	}
}

// TestReconcilePeers_SkipsPruningPeersAlreadyGoing is the same state check on
// the reconcile path. A peer for a node that has gone is pruned, and pruning
// one that EC2 is already removing fails the whole reconcile with
// IncorrectState, so no other node is reconciled either.
func TestReconcilePeers_SkipsPruningPeersAlreadyGoing(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					// Both are for nodes no longer present, so both are stale.
					{PeerAddress: aws.String("10.0.9.9"), RouteServerPeerId: aws.String(stalePeerID), RouteServerEndpointId: aws.String("ep-a1"),
						State: ec2types.RouteServerPeerStateAvailable, Tags: managedTag},
					{PeerAddress: aws.String("10.0.9.8"), RouteServerPeerId: aws.String("peer-stale-going"), RouteServerEndpointId: aws.String("ep-a2"),
						State: ec2types.RouteServerPeerStateDeleting, Tags: managedTag},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1", "ep-a2"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nil); err != nil {
		t.Fatalf("reconcile should not fail on a peer already going: %v", err)
	}

	deleted := make([]string, 0, len(mock.deletePeerCalls))
	for _, call := range mock.deletePeerCalls {
		deleted = append(deleted, aws.ToString(call.RouteServerPeerId))
	}
	if len(deleted) != 1 || deleted[0] != stalePeerID {
		t.Errorf("expected only peer-stale to be pruned, got %v", deleted)
	}
}

// TestReconcilePeers_RecreatesOverADeletedPeer pins the difference between a
// peer that is going and one that has gone.
//
// EC2 keeps returning a peer after it is deleted. A deleted peer holds no
// address, so it must not count as one already existing, or the operator never
// replaces a peer that was removed out from under it and drift is never
// repaired. A deleting peer does still hold its address, so it does count, and
// the replacement waits for the next pass.
func TestReconcilePeers_RecreatesOverADeletedPeer(t *testing.T) {
	managedTag := []ec2types.Tag{{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")}}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{
				RouteServerPeers: []ec2types.RouteServerPeer{
					{PeerAddress: aws.String("10.0.1.10"), RouteServerPeerId: aws.String("peer-gone"), RouteServerEndpointId: aws.String("ep-a1"),
						State: ec2types.RouteServerPeerStateDeleted, Tags: managedTag},
					{PeerAddress: aws.String("10.0.1.10"), RouteServerPeerId: aws.String("peer-going"), RouteServerEndpointId: aws.String("ep-a2"),
						State: ec2types.RouteServerPeerStateDeleting, Tags: managedTag},
				},
			}, nil
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1", "ep-a2"}},
		localASN:      65001,
		clusterID:     "test-cluster",
	}
	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
	}

	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ep-a1's peer is gone, so the node needs a new one there. ep-a2's is
	// still on its way out and holds the address, so nothing is created yet.
	createdOn := make([]string, 0, len(mock.createPeerCalls))
	for _, call := range mock.createPeerCalls {
		createdOn = append(createdOn, aws.ToString(call.RouteServerEndpointId))
	}
	if len(createdOn) != 1 || createdOn[0] != "ep-a1" {
		t.Errorf("expected one peer created on ep-a1, got %v", createdOn)
	}
	if len(mock.deletePeerCalls) != 0 {
		t.Errorf("expected no deletes, got %d", len(mock.deletePeerCalls))
	}
}

func metricValue(t *testing.T, c prometheus.Metric) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	t.Fatal("metric is neither counter nor gauge")
	return 0
}

func TestAWSMetrics_DiscoverErrorIncrements(t *testing.T) {
	before := metricValue(t, awsAPIErrors.WithLabelValues(opDiscover))
	mock := &mockEC2{
		describeRSFunc: func(_ *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
			return nil, errors.New("ec2 API failure")
		},
	}
	p := &Platform{ec2Client: mock, routeServerIDs: []string{"rs-1"}}
	if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	got := metricValue(t, awsAPIErrors.WithLabelValues(opDiscover))
	if got != before+1 {
		t.Fatalf("discover errors: got %v, want %v", got, before+1)
	}
}

func TestAWSMetrics_PeersManagedGauge(t *testing.T) {
	peers := []ec2types.RouteServerPeer{
		testManagedPeer("ep-a1", "10.0.1.10"),
		testManagedPeer("ep-a2", "10.0.1.10"),
		testManagedPeer("ep-b1", "10.0.2.10"),
		testManagedPeer("ep-b2", "10.0.2.10"),
		testManagedPeer("ep-c1", "10.0.3.10"),
		testManagedPeer("ep-c2", "10.0.3.10"),
	}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{RouteServerPeers: peers}, nil
		},
	}
	p := newTestPlatform(mock)
	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
		{Name: "node-b", PrivateIP: "10.0.2.10", Zone: "us-east-1b", ProviderID: "aws:///us-east-1b/i-b"},
		{Name: "node-c", PrivateIP: "10.0.3.10", Zone: "us-east-1c", ProviderID: "aws:///us-east-1c/i-c"},
	}
	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := metricValue(t, awsPeersManaged)
	if got != 6 {
		t.Fatalf("aws_peers_managed: got %v, want 6", got)
	}
}

func TestAWSMetrics_PeersManagedGaugeCountsActualNotDesired(t *testing.T) {
	peers := []ec2types.RouteServerPeer{
		testManagedPeer("ep-a1", "10.0.1.10"),
		testManagedPeer("ep-b1", "10.0.2.10"),
		testManagedPeer("ep-c1", "10.0.3.10"),
	}
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return &ec2.DescribeRouteServerPeersOutput{RouteServerPeers: peers}, nil
		},
	}
	p := newTestPlatform(mock)
	nodes := []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a", ProviderID: "aws:///us-east-1a/i-a"},
		{Name: "node-b", PrivateIP: "10.0.2.10", Zone: "us-east-1b", ProviderID: "aws:///us-east-1b/i-b"},
		{Name: "node-c", PrivateIP: "10.0.3.10", Zone: "us-east-1c", ProviderID: "aws:///us-east-1c/i-c"},
	}
	if err := p.reconcileRouteServerPeers(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := metricValue(t, awsPeersManaged)
	if got != 3 {
		t.Fatalf("aws_peers_managed: got %v, want 3 (actual peers, not desired 6)", got)
	}
}

func testManagedPeer(endpointID, ip string) ec2types.RouteServerPeer {
	return ec2types.RouteServerPeer{
		RouteServerEndpointId: aws.String(endpointID),
		RouteServerPeerId:     aws.String("peer-" + endpointID),
		PeerAddress:           aws.String(ip),
		Tags: []ec2types.Tag{
			{Key: aws.String("managed-by"), Value: aws.String("cudn-bgp-routing-operator/test-cluster")},
		},
	}
}

func TestAWSMetrics_PeerErrorIncrements(t *testing.T) {
	before := metricValue(t, awsAPIErrors.WithLabelValues(opPeer))
	mock := &mockEC2{
		describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
			return nil, errors.New("peers list failed")
		},
	}
	p := &Platform{
		ec2Client:     mock,
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		clusterID:     "test-cluster",
	}
	if err := p.reconcileRouteServerPeers(context.Background(), []platform.RouterNode{
		{Name: "node-a", PrivateIP: "10.0.1.10", Zone: "us-east-1a"},
	}); err == nil {
		t.Fatal("expected error")
	}
	got := metricValue(t, awsAPIErrors.WithLabelValues(opPeer))
	if got != before+1 {
		t.Fatalf("peer errors: got %v, want %v", got, before+1)
	}
}

func TestAWSMetrics_SourceDestErrorIncrements(t *testing.T) {
	before := metricValue(t, awsAPIErrors.WithLabelValues(opSourceDest))
	mock := &mockEC2{
		describeInstFunc: func(_ *ec2.DescribeInstancesInput) (*ec2.DescribeInstancesOutput, error) {
			return nil, errors.New("describe instances failed")
		},
	}
	p := &Platform{ec2Client: mock}
	if err := p.disableSourceDestCheck(context.Background(), []platform.RouterNode{
		{Name: "node-a", ProviderID: "aws:///us-east-1a/i-a"},
	}); err == nil {
		t.Fatal("expected error")
	}
	got := metricValue(t, awsAPIErrors.WithLabelValues(opSourceDest))
	if got != before+1 {
		t.Fatalf("sourcedest errors: got %v, want %v", got, before+1)
	}
}

func TestAWSMetrics_LocalErrorsDoNotIncrement(t *testing.T) {
	discoverBefore := metricValue(t, awsAPIErrors.WithLabelValues(opDiscover))
	sourceBefore := metricValue(t, awsAPIErrors.WithLabelValues(opSourceDest))

	pDiscover := &Platform{
		ec2Client: &mockEC2{
			describeRSFunc: func(_ *ec2.DescribeRouteServersInput) (*ec2.DescribeRouteServersOutput, error) {
				return &ec2.DescribeRouteServersOutput{}, nil
			},
		},
		routeServerIDs: []string{"rs-missing"},
	}
	if _, err := pDiscover.DiscoverEndpoints(context.Background()); err == nil {
		t.Fatal("expected not-found error")
	}
	if got := metricValue(t, awsAPIErrors.WithLabelValues(opDiscover)); got != discoverBefore {
		t.Fatalf("discover errors after not-found: got %v, want %v", got, discoverBefore)
	}

	pSrc := &Platform{ec2Client: &mockEC2{}}
	if err := pSrc.disableSourceDestCheck(context.Background(), []platform.RouterNode{
		{Name: "node-a", ProviderID: "not-aws"},
	}); err == nil {
		t.Fatal("expected providerID error")
	}
	if got := metricValue(t, awsAPIErrors.WithLabelValues(opSourceDest)); got != sourceBefore {
		t.Fatalf("sourcedest errors after bad providerID: got %v, want %v", got, sourceBefore)
	}
}

func TestAWSMetrics_CleanupSetsPeersManagedZero(t *testing.T) {
	awsPeersManaged.Set(3)
	p := &Platform{
		ec2Client: &mockEC2{
			describePeersFunc: func(_ *ec2.DescribeRouteServerPeersInput) (*ec2.DescribeRouteServerPeersOutput, error) {
				return &ec2.DescribeRouteServerPeersOutput{}, nil
			},
		},
		endpointsByAZ: map[string][]string{"us-east-1a": {"ep-a1"}},
		clusterID:     "test-cluster",
	}
	if err := p.deleteAllManagedPeers(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := metricValue(t, awsPeersManaged); got != 0 {
		t.Fatalf("aws_peers_managed after cleanup: got %v, want 0", got)
	}
}
