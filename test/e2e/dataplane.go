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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The data-plane check every cloud suite makes: send a request from a
// client outside the cluster to a pod on every worker, and decide from
// each worker's own interface whether its pod should answer. What
// differs between clouds -- how the client is driven, and what
// "forwarding" means on a node's interface -- stays in the suites; the
// rule itself lives here so it cannot drift between them.

const (
	// The Kubernetes e2e image, as OpenShift mirrors it. netexec's
	// /clientip answers with the source address and port of the request
	// as the pod received it, so the response is itself the evidence that
	// nothing rewrote the source on the way in.
	netexecImage = "quay.io/openshift/community-e2e-images:" +
		"e2e-2-registry-k8s-io-e2e-test-images-agnhost-2-66-1-8pEi_JtcQ76yB5-r"
	netexecName = "netexec"

	// NetexecPort is the port the netexec pods serve on. The client's
	// route to the pods has to admit it.
	NetexecPort = 8080

	// WorkerRoleLabel is carried by every worker and no control-plane node.
	WorkerRoleLabel = "node-role.kubernetes.io/worker"
)

// Destination is one worker and the pod on it, with whether the worker's
// interface forwards traffic whose source is not its own address: Azure's
// enableIPForwarding, AWS's SourceDestCheck switched off.
type Destination struct {
	Node       string
	PodAddress string
	Forwarding bool
}

// EnsureNetexec runs one netexec pod on every worker, in the advertised
// network's namespace, so that every node is a destination and none is
// left to the scheduler's choice.
func EnsureNetexec(ctx context.Context, c client.Client, namespace string) error {
	labels := map[string]string{"app": netexecName}
	f := false
	t := true
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: netexecName, Namespace: namespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{WorkerRoleLabel: ""},
					Containers: []corev1.Container{{
						Name:  netexecName,
						Image: netexecImage,
						Args:  []string{"netexec", fmt.Sprintf("--http-port=%d", NetexecPort)},
						Ports: []corev1.ContainerPort{{ContainerPort: NetexecPort}},
						// Ready only once /clientip answers. A pod that is
						// running but not serving would otherwise pass the
						// "must not answer" check for the wrong reason.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/clientip",
									Port: intstr.FromInt32(NetexecPort),
								},
							},
							PeriodSeconds: 5,
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &f,
							RunAsNonRoot:             &t,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
	if err := c.Create(ctx, ds); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// NetexecPods pairs every worker with its netexec pod's address on the
// advertised network. It fails while any worker lacks a ready pod.
//
// The address comes from the OVN annotation, not status.podIP: on a
// namespace whose primary network is user-defined, podIP is still the
// default network's address, and a probe sent there tests the wrong
// network and fails silently.
func NetexecPods(ctx context.Context, c client.Client, namespace string) ([]corev1.Node, map[string]string, error) {
	workers := &corev1.NodeList{}
	if err := c.List(ctx, workers, client.HasLabels{WorkerRoleLabel}); err != nil {
		return nil, nil, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{"app": netexecName}); err != nil {
		return nil, nil, err
	}
	// While the DaemonSet rolls, a worker can have a terminating pod
	// beside its replacement, and the terminating one can still report
	// Ready, so it is left out.
	byNode := map[string]*corev1.Pod{}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		byNode[pods.Items[i].Spec.NodeName] = &pods.Items[i]
	}
	addresses := map[string]string{}
	for i := range workers.Items {
		node := workers.Items[i].Name
		pod, ok := byNode[node]
		if !ok || !podReady(pod) {
			return nil, nil, fmt.Errorf("no ready netexec pod on worker %s yet", node)
		}
		addr, err := primaryNetworkAddress(pod)
		if err != nil {
			return nil, nil, err
		}
		addresses[node] = addr
	}
	sort.Slice(workers.Items, func(i, j int) bool { return workers.Items[i].Name < workers.Items[j].Name })
	return workers.Items, addresses, nil
}

// podReady reports whether the pod's Ready condition is True, which with
// the readiness probe above means /clientip is answering.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// primaryNetworkAddress reads a pod's address on its primary network
// from the annotation OVN-Kubernetes writes, choosing the entry by role
// rather than by name so no naming convention has to hold.
func primaryNetworkAddress(pod *corev1.Pod) (string, error) {
	raw, ok := pod.Annotations["k8s.ovn.org/pod-networks"]
	if !ok {
		return "", fmt.Errorf("pod %s has no k8s.ovn.org/pod-networks annotation yet", pod.Name)
	}
	var networks map[string]struct {
		IPAddresses []string `json:"ip_addresses"`
		Role        string   `json:"role"`
	}
	if err := json.Unmarshal([]byte(raw), &networks); err != nil {
		return "", fmt.Errorf("pod %s: parsing k8s.ovn.org/pod-networks: %w", pod.Name, err)
	}
	for _, n := range networks {
		if n.Role != "primary" || len(n.IPAddresses) == 0 {
			continue
		}
		ip, _, err := net.ParseCIDR(n.IPAddresses[0])
		if err != nil {
			return "", fmt.Errorf("pod %s: %w", pod.Name, err)
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("pod %s has no address on a primary network", pod.Name)
}

// ProbeScript is the shell a client runs to request /clientip from every
// address, one line per address: "<address> <answer>", with the answer
// empty when the request failed.
func ProbeScript(addresses []string, timeoutSeconds int) string {
	var b strings.Builder
	for _, a := range addresses {
		fmt.Fprintf(&b, "printf '%%s %%s\\n' %s \"$(curl -s -m %d http://%s:%d/clientip)\"\n",
			a, timeoutSeconds, a, NetexecPort)
	}
	return b.String()
}

// ParseProbeOutput reads ProbeScript's output back into address ->
// answer, and fails if any address is missing from it.
func ParseProbeOutput(out string, addresses []string) (map[string]string, error) {
	answers := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		body := ""
		if len(fields) > 1 {
			body = fields[1]
		}
		answers[fields[0]] = body
	}
	for _, a := range addresses {
		if _, ok := answers[a]; !ok {
			return nil, fmt.Errorf("the client reported nothing for %s; its output was:\n%s", a, out)
		}
	}
	return answers, nil
}

// ProbeRound takes one round of probe results out of a client's console
// output, for a client that prints "e2e-probe <round> <address>
// <answer>" for each address and then "e2e-probe <round> end". It
// returns the lines in ProbeScript's form, and whether the end marker
// was seen. A marker is matched anywhere in a line because the console
// carries other writers: a login prompt without a newline puts the next
// line after it.
func ProbeRound(console, round string) (string, bool) {
	prefix := "e2e-probe " + round + " "
	var b strings.Builder
	complete := false
	for _, line := range strings.Split(console, "\n") {
		i := strings.Index(line, prefix)
		if i < 0 {
			continue
		}
		rest := strings.TrimRight(line[i+len(prefix):], "\r")
		if rest == "end" {
			complete = true
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(rest)
	}
	return b.String(), complete
}

// SourceHost strips the port from netexec's "<ip>:<port>" answer.
func SourceHost(answer string) string {
	host, _, err := net.SplitHostPort(answer)
	if err != nil {
		return answer
	}
	return host
}

// CheckAnswers is the rule. A pod's reply leaves through its own node's
// interface with the advertised network's address as its source, and the
// cloud drops it unless that interface forwards. So every pod on a
// forwarding interface must answer, from the client's own address, and
// every pod on one that does not forward must not answer at all.
//
// It can need retrying while routes settle, so it is kept apart from
// CheckCoverage, which does not change during a run.
func CheckAnswers(dests []Destination, answers map[string]string, clientAddress string) error {
	var problems []string
	for _, d := range dests {
		answer := answers[d.PodAddress]
		if d.Forwarding {
			switch {
			case answer == "":
				problems = append(problems, fmt.Sprintf(
					"pod %s on %s did not answer, though its interface forwards", d.PodAddress, d.Node))
			case SourceHost(answer) != clientAddress:
				problems = append(problems, fmt.Sprintf(
					"pod %s on %s saw the request come from %s rather than the client", d.PodAddress, d.Node, answer))
			}
		} else if answer != "" {
			problems = append(problems, fmt.Sprintf(
				"pod %s on %s answered although its interface does not forward; "+
					"the reply path #121 describes has changed", d.PodAddress, d.Node))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "; "))
}

// CheckCoverage requires workers on both sides of the rule: at least one
// that forwards, or nothing can be reachable, and at least one that does
// not, or the "must not answer" half of CheckAnswers checks nothing.
//
// The second happens either because the job no longer leaves a worker
// out of the router set, or because openshift/bgp-cloud-connector#121
// has been fixed and the operator now enables forwarding on non-router
// nodes. The second is meant to fail here: whoever fixes it should
// change the suites to assert that the non-router pod answers.
func CheckCoverage(dests []Destination) error {
	forwarding := 0
	for _, d := range dests {
		if d.Forwarding {
			forwarding++
		}
	}
	switch {
	case forwarding == 0:
		return fmt.Errorf("no worker's interface forwards, so nothing can be reachable")
	case forwarding == len(dests):
		return fmt.Errorf("no worker without forwarding, so the openshift/bgp-cloud-connector#121 case was not exercised: " +
			"either the job no longer leaves a non-router node, or #121 has been fixed; " +
			"if fixed, change this spec to assert that the non-router pod answers")
	}
	return nil
}
