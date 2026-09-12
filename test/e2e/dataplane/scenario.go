/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package dataplane contains provider-independent data-plane assertions.
package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	serverName     = "bgp-dataplane-server"
	serverPort     = 8080
	serverImage    = "registry.k8s.io/e2e-test-images/agnhost:2.53"
	curlImage      = "curlimages/curl:8.12.1"
	connectTimeout = 3 * time.Minute
	connectPoll    = 5 * time.Second
	podNetworksKey = "k8s.ovn.org/pod-networks"
)

// ProbeConfig identifies an external data-plane probe.
type ProbeConfig struct {
	IPv4  string `json:"ipv4"`
	IPv6  string `json:"ipv6,omitempty"`
	Token string `json:"token"`
}

func LoadProbeConfig(path string) (*ProbeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read data-plane probe config: %w", err)
	}
	var cfg ProbeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode data-plane probe config: %w", err)
	}
	if net.ParseIP(cfg.IPv4).To4() == nil {
		return nil, fmt.Errorf("probe config has invalid IPv4 address %q", cfg.IPv4)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("probe config has an empty token")
	}
	return &cfg, nil
}

// Scenario owns the in-cluster data-plane test resources.
type Scenario struct {
	client    client.Client
	namespace string
	probe     ProbeConfig
	subnets   []string
	nodeName  string
	podIPv4   string
}

func New(c client.Client, namespace string, subnets []string, probe ProbeConfig, nodeName string) *Scenario {
	return &Scenario{client: c, namespace: namespace, subnets: subnets, probe: probe, nodeName: nodeName}
}

func (s *Scenario) Setup(ctx context.Context) error {
	allowPrivilegeEscalation := false
	runAsNonRoot := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serverName,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": serverName},
		},
		Spec: corev1.PodSpec{
			NodeName:      s.nodeName,
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:  "server",
				Image: serverImage,
				Args:  []string{"netexec", fmt.Sprintf("--http-port=%d", serverPort)},
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: serverPort}},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					RunAsNonRoot:             &runAsNonRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
	if err := s.client.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create CUDN data-plane server: %w", err)
	}

	deadline := time.Now().Add(connectTimeout)
	for time.Now().Before(deadline) {
		current := &corev1.Pod{}
		if err := s.client.Get(ctx, types.NamespacedName{Name: serverName, Namespace: s.namespace}, current); err == nil {
			if podReady(current) {
				address, err := cudnIPv4(current, s.subnets)
				if err == nil && address != "" {
					s.podIPv4 = address
					return nil
				}
			}
		}
		if err := wait(ctx, connectPoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("CUDN data-plane server did not expose an IPv4 address from %v in annotation %s within %s", s.subnets, podNetworksKey, connectTimeout)
}

// CheckIPv4 checks traffic in both directions.
func (s *Scenario) CheckIPv4(ctx context.Context) error {
	if s.podIPv4 == "" {
		return fmt.Errorf("data-plane scenario is not set up")
	}
	if err := s.podToProbe(ctx); err != nil {
		return fmt.Errorf("CUDN pod to external probe: %w", err)
	}
	if err := s.probeToPod(ctx); err != nil {
		return fmt.Errorf("external probe to CUDN pod %s: %w", s.podIPv4, err)
	}
	return nil
}

func (s *Scenario) Cleanup(ctx context.Context) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: serverName, Namespace: s.namespace}}
	return client.IgnoreNotFound(s.client.Delete(ctx, pod))
}

func (s *Scenario) podToProbe(ctx context.Context) error {
	probeURL := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(s.probe.IPv4, fmt.Sprint(serverPort)),
		Path:   "/health",
	}
	return s.runCurlJob(ctx, "bgp-dataplane-egress-", probeURL.String(), true)
}

func (s *Scenario) probeToPod(ctx context.Context) error {
	probeURL := url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(s.probe.IPv4, fmt.Sprint(serverPort)),
		Path:   "/probe",
	}
	target := "http://" + net.JoinHostPort(s.podIPv4, fmt.Sprint(serverPort)) + "/hostname"
	query := probeURL.Query()
	query.Set("target", target)
	probeURL.RawQuery = query.Encode()

	return s.runCurlJob(ctx, "bgp-dataplane-ingress-", probeURL.String(), false)
}

func (s *Scenario) runCurlJob(ctx context.Context, generateName, requestURL string, requirePodSource bool) error {
	backoffLimit := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: generateName, Namespace: s.namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      s.nodeName,
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "curl",
						Image:   curlImage,
						Command: []string{"sh", "-ec"},
						Args: []string{`i=0
while [ "$i" -lt 36 ]; do
  body="$(curl -fsS --connect-timeout 3 -H "X-BGP-CC-Test-Token: ${TOKEN}" "${REQUEST_URL}")" || true
  if [ "${REQUIRE_POD_SOURCE}" = "true" ]; then
    if [ -n "${body}" ]; then
      printf '%s' "${body}" >/dev/termination-log
      exit 0
    fi
  else
    [ -n "${body}" ] && exit 0
  fi
  i=$((i + 1))
  sleep 5
done
exit 1`},
						Env: []corev1.EnvVar{
							{Name: "REQUEST_URL", Value: requestURL},
							{Name: "REQUIRE_POD_SOURCE", Value: fmt.Sprint(requirePodSource)},
							{Name: "TOKEN", Value: s.probe.Token},
						},
					}},
				},
			},
		},
	}
	if err := s.client.Create(ctx, job); err != nil {
		return fmt.Errorf("create connectivity job: %w", err)
	}
	defer func() {
		policy := metav1.DeletePropagationBackground
		_ = s.client.Delete(context.Background(), job, &client.DeleteOptions{PropagationPolicy: &policy})
	}()

	deadline := time.Now().Add(connectTimeout + 30*time.Second)
	for time.Now().Before(deadline) {
		current := &batchv1.Job{}
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil {
			return fmt.Errorf("read connectivity job: %w", err)
		}
		if current.Status.Succeeded > 0 {
			if requirePodSource {
				return s.verifyJobSource(ctx, current.Name)
			}
			return nil
		}
		if current.Status.Failed > 0 {
			return fmt.Errorf("connectivity job %s failed", current.Name)
		}
		if err := wait(ctx, connectPoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("connectivity job %s did not complete within %s", job.Name, connectTimeout+30*time.Second)
}

func (s *Scenario) verifyJobSource(ctx context.Context, jobName string) error {
	pods := &corev1.PodList{}
	if err := s.client.List(ctx, pods,
		client.InNamespace(s.namespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName},
	); err != nil {
		return fmt.Errorf("list connectivity job pods: %w", err)
	}
	if len(pods.Items) != 1 {
		return fmt.Errorf("connectivity job %s has %d pods, want 1", jobName, len(pods.Items))
	}
	pod := &pods.Items[0]
	assigned, err := cudnIPv4(pod, s.subnets)
	if err != nil {
		return fmt.Errorf("read connectivity pod CUDN address: %w", err)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "curl" || status.State.Terminated == nil {
			continue
		}
		observed := strings.TrimSpace(status.State.Terminated.Message)
		if observed != assigned {
			return fmt.Errorf("external probe observed source %q, want exact CUDN pod address %q", observed, assigned)
		}
		return nil
	}
	return fmt.Errorf("connectivity job %s has no terminated curl container", jobName)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

type podNetwork struct {
	IPAddresses []string `json:"ip_addresses"`
}

// cudnIPv4 selects the address in the BGPRouting subnet.
func cudnIPv4(pod *corev1.Pod, subnets []string) (string, error) {
	var networks map[string]podNetwork
	annotation := pod.Annotations[podNetworksKey]
	if annotation == "" {
		return "", fmt.Errorf("pod has no %s annotation", podNetworksKey)
	}
	if err := json.Unmarshal([]byte(annotation), &networks); err != nil {
		return "", fmt.Errorf("decode %s: %w", podNetworksKey, err)
	}

	var ipv4Networks []*net.IPNet
	for _, subnet := range subnets {
		_, network, err := net.ParseCIDR(subnet)
		if err != nil {
			return "", fmt.Errorf("parse CUDN subnet %q: %w", subnet, err)
		}
		if network.IP.To4() != nil {
			ipv4Networks = append(ipv4Networks, network)
		}
	}
	for _, network := range networks {
		for _, cidr := range network.IPAddresses {
			address, _, err := net.ParseCIDR(cidr)
			if err != nil || address.To4() == nil {
				continue
			}
			for _, expected := range ipv4Networks {
				if expected.Contains(address) {
					return address.String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no pod address belongs to the IPv4 CUDN subnets %v", subnets)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
