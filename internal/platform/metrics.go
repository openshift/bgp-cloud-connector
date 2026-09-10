package platform

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// These metrics are shared by every cloud, so they live here rather than in
// one provider's package: a dashboard or alert can then ask the same question
// of AWS, Azure and GCP and get an answer in the same shape.

// Platform label values — match api/v1alpha1 PlatformType for AWS, Azure, GCP.
// The label set is closed on purpose: a free-form value here would make every
// new spelling a new time series.
const (
	PlatformAWS   = "AWS"
	PlatformAzure = "Azure"
	PlatformGCP   = "GCP"
)

// Cloud API operation labels for cloud_api_errors_total. They name what the
// operator was trying to do, not which API call it made, so the same label
// means the same thing on every cloud.
const (
	// OpDiscover is reading the cloud's BGP endpoints to peer with.
	OpDiscover = "discover"
	// OpPeer is creating, adopting, tagging or removing a peer.
	OpPeer = "peer"
	// OpNodeForwarding is letting a router node forward packets that are not
	// addressed to it: source/dest check on AWS, canIpForward on GCP,
	// enableIPForwarding on Azure.
	OpNodeForwarding = "node_forwarding"
	// OpNCC is a Network Connectivity Center spoke call. GCP only: a VM cannot
	// peer with a Cloud Router until it belongs to a spoke, and no other cloud
	// has that step. It stays a label of its own because a spoke failing and a
	// peer failing are different problems to go and look at.
	OpNCC = "ncc"
)

var (
	cloudAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloud_api_errors_total",
		Help: "Total cloud provider API errors by platform and operation (discover, peer, node_forwarding, ncc)",
	}, []string{"platform", "operation"})

	cloudPeersManaged = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cloud_peers_managed",
		Help: "Number of BGP peers managed by this operator in the cloud after last successful peer reconcile",
	}, []string{"platform"})
)

// Registering with the controller-runtime registry is what puts these on the
// metrics endpoint the manager already serves, so there is no second server to
// run and no scrape config to add.
func init() {
	metrics.Registry.MustRegister(cloudAPIErrors, cloudPeersManaged)
}

// RecordCloudAPIError counts one failed call to the cloud.
//
// Only errors the cloud returned belong here. A provider ID this operator
// cannot parse, or a route server the config names but the cloud does not
// have, is our problem or the user's, and counting it as a cloud failure sends
// whoever is on call to the wrong place.
func RecordCloudAPIError(platform, operation string) {
	cloudAPIErrors.WithLabelValues(platform, operation).Inc()
}

// SetCloudPeersManaged records how many BGP peers this operator holds in the
// cloud. It is a gauge, so each platform writes the whole number after a
// successful reconcile, and writes 0 on cleanup rather than leaving the last
// count sitting there for good.
func SetCloudPeersManaged(platform string, n float64) {
	cloudPeersManaged.WithLabelValues(platform).Set(n)
}

// APIErrorCount and PeersManagedValue read a metric back, for the tests of the
// packages that record them. They hand out values rather than the collectors,
// so nothing outside this file can write to a metric except through the two
// recorders above.
func APIErrorCount(platform, operation string) float64 {
	return metricValue(cloudAPIErrors.WithLabelValues(platform, operation))
}

func PeersManagedValue(platform string) float64 {
	return metricValue(cloudPeersManaged.WithLabelValues(platform))
}

func metricValue(m prometheus.Metric) float64 {
	var out dto.Metric
	// Write only fails on a metric that cannot be collected, which a counter
	// or gauge with fixed labels never is.
	if err := m.Write(&out); err != nil {
		return 0
	}
	if out.Counter != nil {
		return out.Counter.GetValue()
	}
	return out.Gauge.GetValue()
}
