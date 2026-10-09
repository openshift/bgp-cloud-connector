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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OVN-Kubernetes can leave a worker advertising a layer2 primary
// network over BGP while dropping traffic for its pods: without the
// br-ex flows for the network, without the port joining its switch to
// the transit router, or with the gateway router's subnet SNAT in its
// non-advertised form. Restarting the worker's ovnkube-node pod repairs
// all three. Until the fixes in openshift/ovn-kubernetes#3513 ship, the
// data-plane specs restart ovnkube-node once when their probe fails and
// report that they did.

const (
	ovnNamespace       = "openshift-ovn-kubernetes"
	ovnkubeNodeApp     = "ovnkube-node"
	ovnkubeNodeTimeout = 10 * time.Minute

	operatorNamespace  = "openshift-bgp-cloud-connector"
	operatorDeployment = "openshift-bgp-cloud-connector-controller-manager"
)

// networkObjects are recorded with their managedFields, which say who
// last wrote each field and when. Each is read with its own oc call so a
// kind the cluster does not serve costs only its own file.
var networkObjects = []struct {
	file string
	args []string
}{
	{"clusteruserdefinednetworks.yaml", []string{"get", "clusteruserdefinednetworks.k8s.ovn.org"}},
	{"routeadvertisements.yaml", []string{"get", "routeadvertisements.k8s.ovn.org"}},
	{"frrconfigurations.yaml", []string{"get", "frrconfigurations.frrk8s.metallb.io", "-A"}},
	{"network-attachment-definitions.yaml", []string{"get", "network-attachment-definitions.k8s.cni.cncf.io", "-A"}},
	{"network-operator.yaml", []string{"get", "network.operator.openshift.io", "cluster"}},
}

const (
	frrK8sNamespace = "openshift-frr-k8s"
	frrApp          = "frr-k8s"

	// nodeCommandTimeout bounds each command run on a node, so one that
	// hangs costs only its own file.
	nodeCommandTimeout = 2 * time.Minute
)

// ovnkubeNodeCommands are run on each worker, in the named container of
// its ovnkube-node pod. The pod uses the host's network namespace, so ip,
// nft, iptables and conntrack show the node's own state. Every node runs
// its own OVN databases, so the NB and SB commands read that node's view.
var ovnkubeNodeCommands = []struct {
	file      string
	container string
	args      []string
}{
	{"ovn-nb-show.txt", "nbdb", []string{"ovn-nbctl", "--no-leader-only", "show"}},
	{"ovn-nb-dump.txt", "nbdb", []string{"ovsdb-client", "dump", "unix:/var/run/ovn/ovnnb_db.sock"}},
	{"ovn-sb-show.txt", "sbdb", []string{"ovn-sbctl", "--no-leader-only", "show"}},
	{"ovn-sb-lflows.txt", "sbdb", []string{"ovn-sbctl", "--no-leader-only", "lflow-list"}},
	{"ovs-vsctl-show.txt", "ovn-controller", []string{"ovs-vsctl", "show"}},
	{"br-int-flows.txt", "ovn-controller", []string{"ovs-ofctl", "dump-flows", "br-int"}},
	{"br-ex-ports.txt", "ovn-controller", []string{"ovs-ofctl", "dump-ports-desc", "br-ex"}},
	{"datapath-flows.txt", "ovn-controller", []string{"ovs-appctl", "dpctl/dump-flows", "-m"}},
	{"ip-addr.txt", "ovnkube-controller", []string{"ip", "-d", "addr"}},
	{"ip-rule.txt", "ovnkube-controller", []string{"ip", "rule"}},
	{"ip-route.txt", "ovnkube-controller", []string{"ip", "route", "show", "table", "all"}},
	{"ip-neigh.txt", "ovnkube-controller", []string{"ip", "neigh"}},
	{"nft-ruleset.txt", "ovnkube-controller", []string{"nft", "list", "ruleset"}},
	{"iptables.txt", "ovnkube-controller", []string{"iptables-save"}},
	{"conntrack.txt", "ovnkube-controller", []string{"conntrack", "-L"}},
}

// routerCommands are run in each worker's nbdb container for each of its
// logical routers. Whether a network's pod addresses are SNATed on the
// way out is in the NAT of its gateway router, GR_<network>_<node>.
var routerCommands = []struct {
	file string
	verb string
}{
	{"nat", "lr-nat-list"},
	{"routes", "lr-route-list"},
	{"policies", "lr-policy-list"},
}

// frrCommands are run on each worker, in the frr container of its frr-k8s
// pod.
var frrCommands = []struct {
	file string
	args []string
}{
	{"frr-running-config.txt", []string{"vtysh", "-c", "show running-config"}},
	{"frr-bgp-summary.txt", []string{"vtysh", "-c", "show bgp vrf all summary"}},
	{"frr-bgp-ipv4.txt", []string{"vtysh", "-c", "show bgp vrf all ipv4 unicast"}},
	{"frr-routes.txt", []string{"vtysh", "-c", "show ip route vrf all"}},
}

// ProbeAllowingOVNKubeRestart runs check until it passes or timeout
// expires. If it never passes, it records the network state, restarts
// ovnkube-node on every worker, and runs check again for up to timeout;
// only a failure then fails the spec. A restart that was needed is added
// as a report entry, which Ginkgo prints whether or not the spec passes.
//
// The network state is recorded whatever happens: after a first-time
// pass, before a restart, and after the probe that follows it, each in
// its own directory under ${ARTIFACT_DIR}, so a run that needed the
// restart can be compared with one that did not.
func ProbeAllowingOVNKubeRestart(ctx context.Context, c client.Client, workers []string,
	timeout, polling time.Duration, check func(gomega.Gomega)) {
	first := gomega.InterceptGomegaFailure(func() {
		gomega.Eventually(check).WithTimeout(timeout).WithPolling(polling).Should(gomega.Succeed())
	})
	if first == nil {
		recordNetworkState(ctx, c, workers, "network-state-probe-passed")
		return
	}
	ginkgo.GinkgoWriter.Printf("probe failed before any ovnkube-node restart:\n%v\n", first)

	before := recordNetworkState(ctx, c, workers, "network-state-before-ovnkube-restart")

	ginkgo.By("restarting ovnkube-node on every worker, then probing again")
	gomega.Expect(restartOVNKubeNode(ctx, c, workers)).To(gomega.Succeed())
	second := gomega.InterceptGomegaFailure(func() {
		gomega.Eventually(check).WithTimeout(timeout).WithPolling(polling).Should(gomega.Succeed())
	})
	after := recordNetworkState(ctx, c, workers, "network-state-after-ovnkube-restart")
	if second != nil {
		ginkgo.Fail(fmt.Sprintf("the probe failed again after ovnkube-node was restarted on every worker; "+
			"network state before the restart is in %s, after it in %s:\n%v", before, after, second))
	}

	ginkgo.AddReportEntry("ovnkube-node restart needed",
		fmt.Sprintf("the probe failed for %s and passed after ovnkube-node was restarted "+
			"on every worker; network state before the restart is in %s, after it in %s",
			timeout, before, after))
}

// restartOVNKubeNode deletes the ovnkube-node pod on each worker and
// waits until every worker has a ready replacement.
func restartOVNKubeNode(ctx context.Context, c client.Client, workers []string) error {
	old, err := ovnkubeNodePods(ctx, c)
	if err != nil {
		return err
	}
	for _, node := range workers {
		pod, ok := old[node]
		if !ok {
			return fmt.Errorf("no ovnkube-node pod on worker %s", node)
		}
		if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting %s on %s: %w", pod.Name, node, err)
		}
		ginkgo.GinkgoWriter.Printf("deleted %s on %s\n", pod.Name, node)
	}

	var waiting string
	err = wait.PollUntilContextTimeout(ctx, 10*time.Second, ovnkubeNodeTimeout, true,
		func(ctx context.Context) (bool, error) {
			current, err := ovnkubeNodePods(ctx, c)
			if err != nil {
				return false, err
			}
			for _, node := range workers {
				pod, ok := current[node]
				if !ok || pod.UID == old[node].UID || !podReady(pod) {
					waiting = node
					return false, nil
				}
			}
			return true, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for a ready ovnkube-node pod on %s: %w", waiting, err)
	}
	return nil
}

// ovnkubeNodePods maps each node to its ovnkube-node pod, leaving out
// pods that are terminating.
func ovnkubeNodePods(ctx context.Context, c client.Client) (map[string]*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(ovnNamespace),
		client.MatchingLabels{"app": ovnkubeNodeApp}); err != nil {
		return nil, err
	}
	byNode := map[string]*corev1.Pod{}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp != nil {
			continue
		}
		byNode[pods.Items[i].Spec.NodeName] = &pods.Items[i]
	}
	return byNode, nil
}

// recordNetworkState writes, under ${ARTIFACT_DIR}/<name> when
// ARTIFACT_DIR is set and a temporary directory otherwise:
//
//	br-ex-flows/<node>.txt         ovs-ofctl dump-flows br-ex
//	ovnkube-controller/<node>.log  that node's ovnkube-controller log
//	nodes/<node>/<file>            ovnkubeNodeCommands and frrCommands
//	nodes/<node>/<router>-<file>.txt
//	                               routerCommands for each logical router
//	                               on the node
//	operator.log                   the operator's log
//	<kind>.yaml                    networkObjects, with managedFields
//
// and returns the directory. Workers are recorded in parallel. It goes
// through oc, as hack/report-br-ex-flows.sh does, and needs oc on PATH.
// It is diagnostics only: anything that cannot be read is logged and
// skipped.
func recordNetworkState(ctx context.Context, c client.Client, workers []string, name string) string {
	dir := filepath.Join(os.Getenv("ARTIFACT_DIR"), name)
	if os.Getenv("ARTIFACT_DIR") == "" {
		tmp, err := os.MkdirTemp("", name+"-")
		if err != nil {
			ginkgo.GinkgoWriter.Printf("%s not recorded: %v\n", name, err)
			return ""
		}
		dir = tmp
	}

	for _, o := range networkObjects {
		ocToFile(ctx, filepath.Join(dir, o.file), append(o.args, "-o", "yaml", "--show-managed-fields")...)
	}
	ocToFile(ctx, filepath.Join(dir, "operator.log"),
		"-n", operatorNamespace, "logs", "deployment/"+operatorDeployment, "--all-containers", "--timestamps")

	pods, err := ovnkubeNodePods(ctx, c)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("%s: ovnkube-node pods not listed: %v\n", name, err)
		return dir
	}
	frr, err := frrPods(ctx, c)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("%s: frr-k8s pods not listed: %v\n", name, err)
	}
	var wg sync.WaitGroup
	for _, node := range workers {
		pod, ok := pods[node]
		if !ok {
			ginkgo.GinkgoWriter.Printf("%s: no ovnkube-node pod on %s\n", name, node)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordNode(ctx, dir, node, pod.Name, frr[node])
		}()
	}
	wg.Wait()
	ginkgo.GinkgoWriter.Printf("network state written to %s\n", dir)
	return dir
}

// recordNode writes one worker's part of recordNetworkState. frrPod is
// empty when the worker has no frr-k8s pod.
func recordNode(ctx context.Context, dir, node, ovnkubePod, frrPod string) {
	ocToFile(ctx, filepath.Join(dir, "br-ex-flows", node+".txt"),
		"-n", ovnNamespace, "exec", ovnkubePod, "-c", "ovn-controller", "--", "ovs-ofctl", "dump-flows", "br-ex")
	ocToFile(ctx, filepath.Join(dir, "ovnkube-controller", node+".log"),
		"-n", ovnNamespace, "logs", ovnkubePod, "-c", "ovnkube-controller", "--timestamps")

	nodeDir := filepath.Join(dir, "nodes", node)
	inPod := func(container string, args ...string) []string {
		return append([]string{"-n", ovnNamespace, "exec", ovnkubePod, "-c", container, "--"}, args...)
	}
	for _, cmd := range ovnkubeNodeCommands {
		ocToFile(ctx, filepath.Join(nodeDir, cmd.file), inPod(cmd.container, cmd.args...)...)
	}

	routers, err := oc(ctx, inPod("nbdb",
		"ovn-nbctl", "--no-leader-only", "--bare", "--columns=name", "list", "Logical_Router")...)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("%s: logical routers not listed: %v\n", node, err)
	}
	for _, router := range strings.Fields(string(routers)) {
		for _, cmd := range routerCommands {
			ocToFile(ctx, filepath.Join(nodeDir, router+"-"+cmd.file+".txt"),
				inPod("nbdb", "ovn-nbctl", "--no-leader-only", cmd.verb, router)...)
		}
	}

	if frrPod == "" {
		ginkgo.GinkgoWriter.Printf("%s: no frr-k8s pod\n", node)
		return
	}
	for _, cmd := range frrCommands {
		ocToFile(ctx, filepath.Join(nodeDir, cmd.file),
			append([]string{"-n", frrK8sNamespace, "exec", frrPod, "-c", "frr", "--"}, cmd.args...)...)
	}
}

// frrPods maps each node to the name of its frr-k8s pod.
func frrPods(ctx context.Context, c client.Client) (map[string]string, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(frrK8sNamespace),
		client.MatchingLabels{"app": frrApp}); err != nil {
		return nil, err
	}
	byNode := map[string]string{}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			byNode[pod.Spec.NodeName] = pod.Name
		}
	}
	return byNode, nil
}

// oc runs oc with args, for at most nodeCommandTimeout, and returns its
// standard output.
func oc(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "oc", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("oc %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// ocToFile runs oc with args and writes its standard output to path,
// creating path's directory. A failure is logged, not returned.
func ocToFile(ctx context.Context, path string, args ...string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		ginkgo.GinkgoWriter.Printf("%s not recorded: %v\n", path, err)
		return
	}
	out, err := oc(ctx, args...)
	if err != nil {
		ginkgo.GinkgoWriter.Printf("%s not recorded: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		ginkgo.GinkgoWriter.Printf("%s not recorded: %v\n", path, err)
	}
}
