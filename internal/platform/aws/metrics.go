package aws

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	opDiscover   = "discover"
	opPeer       = "peer"
	opSourceDest = "sourcedest"
)

var (
	awsAPIErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "aws_api_errors_total",
		Help: "Total AWS EC2 API errors by operation (discover, peer, sourcedest)",
	}, []string{"operation"})

	awsPeersManaged = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aws_peers_managed",
		Help: "Number of tagged Route Server peers in AWS matching desired router nodes after last successful peer reconcile",
	})
)

func init() {
	metrics.Registry.MustRegister(awsAPIErrors, awsPeersManaged)
}

func recordAWSAPIError(operation string) {
	awsAPIErrors.WithLabelValues(operation).Inc()
}
