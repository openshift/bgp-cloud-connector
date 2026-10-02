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

package azure_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The Kubernetes e2e image, as OpenShift mirrors it. netexec's
	// /clientip answers with the source address and port of the request
	// as the pod received it, so the response is itself the evidence that
	// nothing rewrote the source on the way in.
	netexecImage = "quay.io/openshift/community-e2e-images:" +
		"e2e-2-registry-k8s-io-e2e-test-images-agnhost-2-66-1-8pEi_JtcQ76yB5-r"
	netexecName = "netexec"
	netexecPort = 8080

	// How long the client VM waits for one pod. A pod that is reachable
	// answers in milliseconds; this only bounds the wait on one that is
	// not.
	probeTimeoutSeconds = 10

	// The label every worker carries and no control-plane node does.
	workerRoleLabel = "node-role.kubernetes.io/worker"

	// How long one az invocation may take. A run-command answered in
	// thirty to forty seconds when measured, and Azure allows one to run
	// for up to ninety minutes; Eventually stops retrying at its own
	// timeout but cannot interrupt a call already in progress, so each
	// call carries its own deadline.
	azTimeout = 3 * time.Minute
)

// clientVM names the VM hack/azure/create-client-vm.sh builds. Derived
// rather than passed, so the script and the suite agree by construction
// on a cluster neither was told about.
func clientVM() (name, resourceGroup string) {
	return clusterID + "-e2e-client", bgpConfig.Spec.Azure.ResourceGroup
}

// az runs the Azure CLI against the profile's subscription and returns
// its stdout. The suite drives the client VM through `az vm run-command`
// rather than through a new SDK module: the CLI is already what the CI
// step logs in with, and the call goes through the VM agent rather than
// the network, so the VM needs no public address and no inbound rule.
func az(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, azTimeout)
	defer cancel()
	args = append(args, "--subscription", bgpConfig.Spec.Azure.SubscriptionID)
	cmd := exec.CommandContext(ctx, "az", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("az %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// clientVMAddress reads the client VM's private address from Azure. The
// pods are expected to report exactly this as the source of every
// request.
func clientVMAddress(ctx context.Context) (string, error) {
	name, rg := clientVM()
	addr, err := az(ctx, "vm", "list-ip-addresses", "-g", rg, "-n", name,
		"--query", "[0].virtualMachine.network.privateIpAddresses[0]", "-o", "tsv")
	if err != nil {
		return "", err
	}
	if net.ParseIP(addr) == nil {
		return "", fmt.Errorf("no client VM %s in %s (got %q); create it with hack/azure/create-client-vm.sh", name, rg, addr)
	}
	return addr, nil
}

// ensureNetexec runs one netexec pod on every worker, in the advertised
// network's namespace, so that every node is a destination and none is
// left to the scheduler's choice.
func ensureNetexec(ctx context.Context, namespace string) {
	labels := map[string]string{"app": netexecName}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: netexecName, Namespace: namespace},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{workerRoleLabel: ""},
					Containers: []corev1.Container{{
						Name:  netexecName,
						Image: netexecImage,
						Args:  []string{"netexec", fmt.Sprintf("--http-port=%d", netexecPort)},
						Ports: []corev1.ContainerPort{{ContainerPort: netexecPort}},
						// Ready only once /clientip answers. A pod that is
						// running but not serving would otherwise pass the
						// "must not answer" check for the wrong reason.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/clientip",
									Port: intstr.FromInt32(netexecPort),
								},
							},
							PeriodSeconds: 5,
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: to.Ptr(false),
							RunAsNonRoot:             to.Ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
				},
			},
		},
	}
	err := k8sClient.Create(ctx, ds)
	if apierrors.IsAlreadyExists(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
}

// destination is one worker and the pod on it, with what Azure says
// about that worker's interface.
type destination struct {
	Node       string
	PodAddress string
	Forwarding bool
}

// destinations pairs every worker with its netexec pod's address on the
// advertised network, and reads the worker's interface from Azure.
//
// The address comes from the OVN annotation, not status.podIP: on a
// namespace whose primary network is user-defined, podIP is still the
// default network's address, and a probe sent there tests the wrong
// network and fails silently.
func destinations(ctx context.Context, namespace string) ([]destination, error) {
	workers := &corev1.NodeList{}
	if err := k8sClient.List(ctx, workers, client.HasLabels{workerRoleLabel}); err != nil {
		return nil, err
	}
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{"app": netexecName}); err != nil {
		return nil, err
	}
	byNode := map[string]*corev1.Pod{}
	for i := range pods.Items {
		byNode[pods.Items[i].Spec.NodeName] = &pods.Items[i]
	}

	out := make([]destination, 0, len(workers.Items))
	for i := range workers.Items {
		node := &workers.Items[i]
		pod, ok := byNode[node.Name]
		if !ok || !podReady(pod) {
			return nil, fmt.Errorf("no ready netexec pod on worker %s yet", node.Name)
		}
		addr, err := primaryNetworkAddress(pod)
		if err != nil {
			return nil, err
		}
		nic, err := nicForNode(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("reading the interface of %s: %w", node.Name, err)
		}
		if nic == nil || nic.Properties == nil {
			return nil, fmt.Errorf("no interface found for %s", node.Name)
		}
		out = append(out, destination{
			Node:       node.Name,
			PodAddress: addr,
			Forwarding: nic.Properties.EnableIPForwarding != nil && *nic.Properties.EnableIPForwarding,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
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

// probe sends one request to every address from the client VM, in a
// single run-command, and returns what each address answered. An
// address that did not answer maps to "".
//
// One invocation rather than one per address because each costs about
// thirty seconds of agent round trip, which would dominate the run.
func probe(ctx context.Context, addresses []string) (map[string]string, error) {
	var script strings.Builder
	for _, a := range addresses {
		// Every line is "<address> <body>", with the body empty when the
		// request failed, so the parse below cannot confuse a failure
		// with an answer.
		fmt.Fprintf(&script, "printf '%%s %%s\\n' %s \"$(curl -s -m %d http://%s:%d/clientip)\"\n",
			a, probeTimeoutSeconds, a, netexecPort)
	}
	name, rg := clientVM()
	msg, err := az(ctx, "vm", "run-command", "invoke", "-g", rg, "-n", name,
		"--command-id", "RunShellScript", "--scripts", script.String(),
		"--query", "value[0].message", "-o", "tsv")
	if err != nil {
		return nil, err
	}
	GinkgoWriter.Printf("client VM said:\n%s\n", msg)

	// The message is the agent's report: a status line, then the
	// script's stdout between [stdout] and [stderr].
	answers := map[string]string{}
	inStdout := false
	for _, line := range strings.Split(msg, "\n") {
		switch strings.TrimSpace(line) {
		case "[stdout]":
			inStdout = true
			continue
		case "[stderr]":
			inStdout = false
			continue
		}
		if !inStdout {
			continue
		}
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
			return nil, fmt.Errorf("the client VM reported nothing for %s; run-command output was:\n%s", a, msg)
		}
	}
	return answers, nil
}

// sourceHost strips the port from netexec's "<ip>:<port>" answer.
func sourceHost(answer string) string {
	host, _, err := net.SplitHostPort(answer)
	if err != nil {
		return answer
	}
	return host
}
