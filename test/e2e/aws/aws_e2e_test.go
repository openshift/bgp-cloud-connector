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

package aws_e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

const (
	frrNamespace           = "openshift-frr-k8s"
	frrConfigNamePrefix    = "bgp-cc-"
	routeAdvertisementName = "bgp-cc-route-advertisements"
	labelManagedBy         = "app.kubernetes.io/managed-by"
	labelManagedByVal      = "bgp-cloud-connector"

	reconcileTimeout = 6 * time.Minute
	pollInterval     = 10 * time.Second
	// A route server peer takes minutes to reach available, and EC2 refuses
	// state-changing calls on one that has not.
	peerSettleTimeout = 15 * time.Minute
)

var _ = Describe("AWS E2E", Ordered, func() {

	// ---------------------------------------------------------------
	// E2E-AWS-01: Full stack reconcile
	// ---------------------------------------------------------------
	Context("E2E-AWS-01: Full stack reconcile", func() {
		It("should apply CRs and reach Ready state with all AWS resources", func(ctx context.Context) {
			By("applying BGPCloudConfiguration CR")
			configCR := bgpConfig.DeepCopy()
			configCR.ResourceVersion = ""
			Expect(e2e.CreateOrReuse(ctx, k8sClient, configCR, e2e.ReuseCRs)).To(Succeed())

			By("waiting for config phase=Ready")
			Eventually(func(g Gomega) {
				cfg := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
				g.Expect(cfg.Status.Phase).To(Equal(networkingapi.PhaseReady))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying Network/cluster ownership is External (FRR pre-enabled externally)")
			Expect(e2e.CheckConfigNetworkOwnershipExternal(ctx, k8sClient, configCR.Name)).To(Succeed())

			By("verifying FRRConfigurations exist")
			azCount := len(endpointsByAZ)
			Expect(azCount).To(BeNumerically(">", 0), "expected at least 1 AZ from discovery")
			for i := 1; i <= azCount; i++ {
				frrCfg := &unstructured.Unstructured{}
				frrCfg.SetGroupVersionKind(schema.GroupVersionKind{
					Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration",
				})
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      frrConfigNamePrefix + itoa(i),
					Namespace: frrNamespace,
				}, frrCfg)).To(Succeed(), "FRRConfiguration %d should exist", i)
			}

			By("verifying the discovered peering plan is reported")
			cfgFresh := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfgFresh)).To(Succeed())
			Expect(cfgFresh.Status.PeerGroups).NotTo(BeEmpty(),
				"status.peerGroups should report the plan the operator discovered")
			for _, pg := range cfgFresh.Status.PeerGroups {
				Expect(pg.Key).NotTo(BeEmpty(), "each peer group should name its zone")
				Expect(pg.Neighbors).NotTo(BeEmpty(), "each peer group should carry its neighbours")
			}

			By("verifying Route Server peers exist per AZ")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty(), "expected at least 1 router node")

			nodeIPsByAZ := make(map[string]map[string]bool)
			for _, n := range nodes {
				az := n.Labels["topology.kubernetes.io/zone"]
				if nodeIPsByAZ[az] == nil {
					nodeIPsByAZ[az] = make(map[string]bool)
				}
				nodeIPsByAZ[az][nodeInternalIP(&n)] = true
			}

			peers, err := allManagedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peers).NotTo(BeEmpty(), "expected managed route server peers")

			By("verifying SourceDestCheck=false on all router nodes")
			for _, n := range nodes {
				instanceID, _, err := awsplatform.ParseProviderID(n.Spec.ProviderID)
				Expect(err).NotTo(HaveOccurred())
				inst, err := describeInstance(ctx, instanceID)
				Expect(err).NotTo(HaveOccurred())
				eni := primaryENI(inst)
				Expect(eni).NotTo(BeNil(), "primary ENI should exist for %s", n.Name)
				Expect(aws.ToBool(eni.SourceDestCheck)).To(BeFalse(),
					"SourceDestCheck should be disabled on %s", n.Name)
			}

			By("creating labeled namespace for ClusterUDN")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: bgpRouting.Spec.Network.Name,
					Labels: map[string]string{
						"k8s.ovn.org/primary-user-defined-network": "",
						"cluster-udn": bgpRouting.Spec.Network.Name,
					},
				},
			}
			Expect(e2e.CreateOrReuse(ctx, k8sClient, ns, e2e.ReuseCRs)).To(Succeed())

			By("applying BGPRouting CR")
			routingCR := bgpRouting.DeepCopy()
			routingCR.ResourceVersion = ""
			Expect(e2e.CreateOrReuse(ctx, k8sClient, routingCR, e2e.ReuseCRs)).To(Succeed())

			By("waiting for routing phase=Ready")
			Eventually(func(g Gomega) {
				rt := &networkingapi.BGPRouting{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: routingCR.Name}, rt)).To(Succeed())
				g.Expect(rt.Status.Phase).To(Equal(networkingapi.PhaseReady))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying routing resources: ClusterUDN, RouteAdvertisements")

			cudn := &unstructured.Unstructured{}
			cudn.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork",
			})
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "cluster-udn-" + bgpRouting.Spec.Network.Name,
			}, cudn)).To(Succeed())

			ra := &unstructured.Unstructured{}
			ra.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements",
			})
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: routeAdvertisementName}, ra)).To(Succeed())

			By("verifying FRR pods show established BGP sessions")
			frrPods := &corev1.PodList{}
			Expect(k8sClient.List(ctx, frrPods,
				client.InNamespace(frrNamespace),
				client.MatchingLabels{"app": "frr-k8s"},
			)).To(Succeed())
			Expect(frrPods.Items).NotTo(BeEmpty(), "FRR pods should be running")
			for _, pod := range frrPods.Items {
				Expect(pod.Status.Phase).To(Equal(corev1.PodRunning), "FRR pod %s should be Running", pod.Name)
			}
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-02: Node add/replace/remove cycle
	// ---------------------------------------------------------------
	Context("E2E-AWS-02: Node lifecycle", func() {
		var initialNodes []corev1.Node
		var initialPeerCount int

		BeforeAll(func(ctx context.Context) {
			var err error
			initialNodes, err = routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			peers, err := allManagedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			initialPeerCount = len(peers)
		})

		It("should create peers for new router nodes and remove stale ones", func(ctx context.Context) {
			By("recording initial state")
			GinkgoWriter.Printf("initial router nodes: %d, initial peers: %d\n",
				len(initialNodes), initialPeerCount)

			By("waiting for operator to reconcile (next 5m cycle)")
			// The operator re-reconciles every 5 minutes; after a node change the
			// watcher should trigger sooner. We poll until the peer set matches
			// the current node set.
			Eventually(func(g Gomega) {
				nodes, err := routerNodes(ctx)
				g.Expect(err).NotTo(HaveOccurred())

				expectedIPs := make(map[string]bool)
				for _, n := range nodes {
					expectedIPs[nodeInternalIP(&n)] = true
				}

				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred())

				peerIPs := make(map[string]bool)
				for _, p := range peers {
					if p.PeerAddress != nil {
						peerIPs[*p.PeerAddress] = true
					}
				}

				// Every router node IP should have peers
				for ip := range expectedIPs {
					g.Expect(peerIPs).To(HaveKey(ip), "missing peer for node IP %s", ip)
				}

				// SourceDestCheck should be disabled
				for _, n := range nodes {
					instanceID, _, err := awsplatform.ParseProviderID(n.Spec.ProviderID)
					g.Expect(err).NotTo(HaveOccurred())
					inst, err := describeInstance(ctx, instanceID)
					g.Expect(err).NotTo(HaveOccurred())
					eni := primaryENI(inst)
					g.Expect(eni).NotTo(BeNil())
					g.Expect(aws.ToBool(eni.SourceDestCheck)).To(BeFalse())
				}
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-03: Self-healing — peer manually deleted
	// ---------------------------------------------------------------
	Context("E2E-AWS-03: Route Server peer manually deleted", func() {
		It("should recreate the deleted peer within the reconcile window", func(ctx context.Context) {
			// EC2 refuses a delete on a peer that has not finished being
			// created, so this waits for one to settle rather than taking
			// whichever comes back first. Creation takes minutes, and the
			// previous spec has only just made these.
			By("waiting for a managed peer to reach available")
			var victimID, victimIP string
			Eventually(func(g Gomega) {
				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred(), "failed to list managed peers")
				for _, peer := range peers {
					if peer.State == ec2types.RouteServerPeerStateAvailable {
						victimID = aws.ToString(peer.RouteServerPeerId)
						victimIP = aws.ToString(peer.PeerAddress)
						return
					}
				}
				g.Expect(victimID).NotTo(BeEmpty(), "no managed peer has reached available yet")
			}).WithTimeout(peerSettleTimeout).WithPolling(pollInterval).Should(Succeed())
			GinkgoWriter.Printf("deleting peer %s (IP %s)\n", victimID, victimIP)

			By("deleting the peer via EC2 API")
			_, err := ec2Client.DeleteRouteServerPeer(ctx, &ec2.DeleteRouteServerPeerInput{
				RouteServerPeerId: aws.String(victimID),
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for operator to recreate it")
			Eventually(func(g Gomega) {
				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred(), "failed to list managed peers")

				// Address alone is not enough. listManagedPeers keeps
				// peers in Deleting, so the one just deleted is still
				// listed under the same address and this would match it
				// and pass without the operator having done anything.
				// The replacement is a different peer id, and available.
				found := false
				for _, p := range peers {
					if aws.ToString(p.PeerAddress) == victimIP &&
						aws.ToString(p.RouteServerPeerId) != victimID &&
						p.State == ec2types.RouteServerPeerStateAvailable {
						found = true
						break
					}
				}
				g.Expect(found).To(BeTrue(), "peer for IP %s should be recreated", victimIP)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-04: Self-healing — SourceDestCheck re-enabled
	// ---------------------------------------------------------------
	Context("E2E-AWS-04: SourceDestCheck manually re-enabled", func() {
		It("should disable SourceDestCheck again within the reconcile window", func(ctx context.Context) {
			By("finding a router node to tamper with")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())

			node := nodes[0]
			instanceID, _, err := awsplatform.ParseProviderID(node.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())

			inst, err := describeInstance(ctx, instanceID)
			Expect(err).NotTo(HaveOccurred())
			eni := primaryENI(inst)
			Expect(eni).NotTo(BeNil())
			eniID := aws.ToString(eni.NetworkInterfaceId)

			By("re-enabling SourceDestCheck via EC2 API")
			_, err = ec2Client.ModifyNetworkInterfaceAttribute(ctx,
				&ec2.ModifyNetworkInterfaceAttributeInput{
					NetworkInterfaceId: aws.String(eniID),
					SourceDestCheck:    &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
				})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for operator to disable it again")
			Eventually(func(g Gomega) {
				inst, err := describeInstance(ctx, instanceID)
				g.Expect(err).NotTo(HaveOccurred())
				eni := primaryENI(inst)
				g.Expect(eni).NotTo(BeNil())
				g.Expect(aws.ToBool(eni.SourceDestCheck)).To(BeFalse(),
					"SourceDestCheck should be disabled on %s", node.Name)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-06: traffic from outside the cluster
	//
	// Before E2E-AWS-05, which removes everything this needs. The
	// container is Ordered, so position is what decides.
	// ---------------------------------------------------------------
	Context("E2E-AWS-06: Traffic from outside the cluster", func() {
		It("should reach a pod on every worker whose interface forwards, "+
			"from the client instance's own address, and no other", func(ctx context.Context) {
			namespace := bgpRouting.Spec.Network.Name

			By("finding the client instance")
			instanceID, clientAddress, err := clientInstance(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("running a netexec pod on every worker")
			Expect(e2e.EnsureNetexec(ctx, k8sClient, namespace)).To(Succeed())

			// Each node's expected result comes from its interface as EC2
			// reports it, not from its labels. A pod's reply leaves
			// through its own node's interface with the advertised
			// network's address as its source, and EC2 drops that unless
			// source/destination checking is off on that interface. A node
			// that left the router set keeps whatever setting it was
			// given, so labels alone cannot say which nodes answer.
			var dests []e2e.Destination
			Eventually(func(g Gomega) {
				found, findErr := destinations(ctx, namespace)
				g.Expect(findErr).NotTo(HaveOccurred())
				dests = found
			}).WithTimeout(5 * time.Minute).WithPolling(pollInterval).Should(Succeed())

			routers, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			isRouter := map[string]bool{}
			for _, n := range routers {
				isRouter[n.Name] = true
			}
			addresses := make([]string, 0, len(dests))
			workers := make([]string, 0, len(dests))
			for _, d := range dests {
				GinkgoWriter.Printf("worker %s: pod %s, router=%v, forwarding=%v\n",
					d.Node, d.PodAddress, isRouter[d.Node], d.Forwarding)
				addresses = append(addresses, d.PodAddress)
				workers = append(workers, d.Node)
			}

			// Longer than on Azure: AWS reported the peers up and
			// programmed the learned route about six minutes after the
			// CRs were applied, measured once. The specs before this one
			// normally cover that, but the probe should not depend on it.
			By(fmt.Sprintf("requesting /clientip from every pod via the client instance at %s", clientAddress))
			e2e.ProbeRecordingNetworkState(ctx, k8sClient, workers, 10*time.Minute, 30*time.Second, func(g Gomega) {
				answers, probeErr := probe(ctx, instanceID, addresses)
				g.Expect(probeErr).NotTo(HaveOccurred())
				g.Expect(e2e.CheckAnswers(dests, answers, clientAddress)).To(Succeed())
			})

			// After the probe, so that a run which trips this has already
			// shown whether every pod it could reach answered.
			Expect(e2e.CheckCoverage(dests)).To(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-07: forwardingNodeSelector
	//
	// After E2E-AWS-06 and before E2E-AWS-05. 05 removes the CRs this
	// spec needs, and 06 has already waited for the advertised route, so
	// the probe here only waits for the replies. The spec turns
	// SourceDestCheck off on every worker and, as the operator never turns
	// it back on, turns it on again when it ends on the workers that
	// started with it. The container is Ordered, so position decides.
	// ---------------------------------------------------------------
	Context("E2E-AWS-07: forwardingNodeSelector", func() {
		It("should answer from every worker once the selector matches them, without adding peers", func(ctx context.Context) {
			namespace := bgpRouting.Spec.Network.Name

			By("finding the client instance")
			instanceID, clientAddress, err := clientInstance(ctx)
			Expect(err).NotTo(HaveOccurred())

			By("making sure a netexec pod is running on every worker")
			Expect(e2e.EnsureNetexec(ctx, k8sClient, namespace)).To(Succeed())

			By("checking that at least one worker interface does not forward yet")
			var initial []e2e.Destination
			Eventually(func(g Gomega) {
				found, findErr := destinations(ctx, namespace)
				g.Expect(findErr).NotTo(HaveOccurred())
				initial = found
			}).WithTimeout(5 * time.Minute).WithPolling(pollInterval).Should(Succeed())
			Expect(slices.ContainsFunc(initial, func(d e2e.Destination) bool { return !d.Forwarding })).To(BeTrue(),
				"every worker interface already forwards, so the selector cannot be seen turning forwarding on; "+
					"run this spec on a cluster where a worker still has SourceDestCheck on")

			var restore []string
			for _, d := range initial {
				if !d.Forwarding {
					restore = append(restore, d.Node)
				}
			}
			DeferCleanup(func(ctx context.Context) {
				By("unsetting forwardingNodeSelector and waiting for the cloud pass that saw it")
				gen, clearErr := e2e.ClearForwardingSelector(ctx, k8sClient, bgpConfig.Name)
				Expect(clearErr).NotTo(HaveOccurred())
				Eventually(func() error {
					return e2e.CloudPassDone(ctx, k8sClient, bgpConfig.Name, gen)
				}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

				By("turning SourceDestCheck back on for the workers that started without forwarding")
				for _, name := range restore {
					node := &corev1.Node{}
					Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, node)).To(Succeed())
					Expect(setSourceDestCheck(ctx, node, true)).To(Succeed())
				}
			})

			before, err := allManagedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			beforeIDs := peerIDs(before)

			By("labelling the workers to select")
			selector, err := e2e.LabelForwardingWorkers(ctx, k8sClient)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func(ctx context.Context) {
				Expect(e2e.RemoveForwardingLabel(ctx, k8sClient)).To(Succeed())
			})

			By("setting forwardingNodeSelector to the labelled workers")
			cfg := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)).To(Succeed())
			patch := client.MergeFrom(cfg.DeepCopy())
			cfg.Spec.ForwardingNodeSelector = selector
			Expect(k8sClient.Patch(ctx, cfg, patch)).To(Succeed())
			patched := cfg.Generation

			By("waiting for the cloud pass that saw the selector to finish")
			Eventually(func(g Gomega) {
				fresh := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, fresh)).To(Succeed())
				g.Expect(fresh.Status.Phase).To(Equal(networkingapi.PhaseReady))
				cond := meta.FindStatusCondition(fresh.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(cond.ObservedGeneration).To(Equal(patched))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting until every worker interface forwards")
			var dests []e2e.Destination
			Eventually(func(g Gomega) {
				found, findErr := destinations(ctx, namespace)
				g.Expect(findErr).NotTo(HaveOccurred())
				g.Expect(found).NotTo(BeEmpty())
				for _, d := range found {
					g.Expect(d.Forwarding).To(BeTrue(), "SourceDestCheck still enabled on %s", d.Node)
				}
				dests = found
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			// At least one worker must not already be a speaker, or this selector adds nobody.
			routers, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(routers).NotTo(BeEmpty())
			Expect(len(dests)).To(BeNumerically(">", len(routers)))

			// Workers matched only by forwardingNodeSelector must not be added as peers.
			after, err := allManagedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peerIDs(after)).To(Equal(beforeIDs))

			addresses := make([]string, 0, len(dests))
			workers := make([]string, 0, len(dests))
			for _, d := range dests {
				addresses = append(addresses, d.PodAddress)
				workers = append(workers, d.Node)
			}

			// The advertised prefix is already programmed. The remaining wait is the reply from the other workers.
			By(fmt.Sprintf("requesting /clientip from every pod via the client instance at %s", clientAddress))
			e2e.ProbeRecordingNetworkState(ctx, k8sClient, workers, 2*time.Minute, 30*time.Second, func(g Gomega) {
				answers, probeErr := probe(ctx, instanceID, addresses)
				g.Expect(probeErr).NotTo(HaveOccurred())
				g.Expect(e2e.CheckAnswers(dests, answers, clientAddress)).To(Succeed())
			})
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-08: one node cannot disable SourceDestCheck
	//
	// Late in the ordered suite because it deliberately disrupts one
	// established peer. E2E-AWS-05 is the only spec after it and removes
	// the configuration and all managed peers.
	// ---------------------------------------------------------------
	Context("E2E-AWS-08: Isolate SourceDestCheck failure", func() {
		It("should skip only the failed node and recover after the deny is removed", func(ctx context.Context) {
			const denyPolicyName = "bgpcc-e2e-deny-source-dest-check"

			By("choosing one established router node as the forwarding failure")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes)).To(BeNumerically(">=", 2), "the isolation check needs one failed and one healthy router node")
			victim, control := nodes[0], nodes[1]
			touchedNodes := []*corev1.Node{&victim, &control}
			victimIP := nodeInternalIP(&victim)
			controlIP := nodeInternalIP(&control)
			instanceID, _, err := awsplatform.ParseProviderID(victim.Spec.ProviderID)
			Expect(err).NotTo(HaveOccurred())
			instance, err := describeInstance(ctx, instanceID)
			Expect(err).NotTo(HaveOccurred())
			eni := primaryENI(instance)
			Expect(eni).NotTo(BeNil())
			eniID := aws.ToString(eni.NetworkInterfaceId)

			op, err := operatorPrincipal(ctx)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("failure target node=%s eni=%s operator=%s:%s\n", victim.Name, eniID, op.Kind, op.ARN)

			cfg := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)).To(Succeed())
			Expect(cfg.Spec.ForwardingNodeSelector).To(BeEmpty(), "forwarding selector cleanup did not complete")
			originalForwardingSelector := maps.Clone(cfg.Spec.ForwardingNodeSelector)
			selectorChanged := false
			policyAttached := false

			restoreForwardingSelector := func(ctx context.Context) int64 {
				fresh := &networkingapi.BGPCloudConfiguration{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, fresh)).To(Succeed())
				patch := client.MergeFrom(fresh.DeepCopy())
				fresh.Spec.ForwardingNodeSelector = maps.Clone(originalForwardingSelector)
				Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
				return fresh.Generation
			}
			sourceDestCheck := func(g Gomega, node *corev1.Node) bool {
				id, _, err := awsplatform.ParseProviderID(node.Spec.ProviderID)
				g.Expect(err).NotTo(HaveOccurred())
				instance, err := describeInstance(ctx, id)
				g.Expect(err).NotTo(HaveOccurred())
				eni := primaryENI(instance)
				g.Expect(eni).NotTo(BeNil())
				return aws.ToBool(eni.SourceDestCheck)
			}

			// Register cleanup before the first mutation so no failure can leave the deny attached.
			DeferCleanup(func(ctx context.Context) {
				if policyAttached {
					By("removing the temporary SourceDestCheck deny")
					Expect(deleteInlinePolicy(ctx, op, denyPolicyName)).To(Succeed())
				}

				for _, node := range touchedNodes {
					Expect(setSourceDestCheck(ctx, node, false)).To(Succeed())
				}
				if selectorChanged {
					restoreForwardingSelector(ctx)
				}
			})

			before, err := allManagedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			oldPeers := map[string]map[string]bool{victimIP: {}, controlIP: {}}
			for _, peer := range before {
				if peer.State != ec2types.RouteServerPeerStateAvailable {
					continue
				}
				if peers := oldPeers[aws.ToString(peer.PeerAddress)]; peers != nil {
					peers[aws.ToString(peer.RouteServerPeerId)] = true
				}
			}
			Expect(oldPeers[victimIP]).NotTo(BeEmpty(), "victim %s has no available peer to remove", victim.Name)
			Expect(oldPeers[controlIP]).NotTo(BeEmpty(), "control %s has no available peer to preserve", control.Name)

			By("denying SourceDestCheck changes only on the victim ENI")
			eniARN := fmt.Sprintf("arn:%s:ec2:%s:%s:network-interface/%s", op.Partition, cfg.Spec.AWS.Region, op.Account, eniID)
			err = putInlinePolicy(ctx, op, denyPolicyName, eniARN)
			Expect(err).NotTo(HaveOccurred(), "cannot attach inline policy to operator %s", op.Kind)
			policyAttached = true

			By("turning SourceDestCheck on for the victim and one healthy control")
			for _, node := range touchedNodes {
				Expect(setSourceDestCheck(ctx, node, true)).To(Succeed(), "E2E and operator identities must differ")
			}

			By("changing generation without changing the effective router set")
			patch := client.MergeFrom(cfg.DeepCopy())
			cfg.Spec.ForwardingNodeSelector = maps.Clone(cfg.Spec.RouterNodeSelector)
			Expect(k8sClient.Patch(ctx, cfg, patch)).To(Succeed())
			selectorChanged = true
			failureGeneration := cfg.Generation

			By("waiting for the partial cloud pass to report the victim")
			Eventually(func(g Gomega) {
				// Re-arm the fault until the IAM deny reaches the operator identity.
				g.Expect(setSourceDestCheck(ctx, &victim, true)).To(Succeed())
				fresh := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, fresh)).To(Succeed())
				g.Expect(fresh.Status.Phase).To(Equal(networkingapi.PhaseDegraded))
				cond := meta.FindStatusCondition(fresh.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Reason).To(Equal("CloudReconcileFailed"))
				g.Expect(cond.ObservedGeneration).To(Equal(failureGeneration))
				g.Expect(cond.Message).To(ContainSubstring(victim.Name))
				g.Expect(cond.Message).To(ContainSubstring(eniID))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying the victim remains enabled while the healthy control is repaired")
			Eventually(func(g Gomega) {
				g.Expect(sourceDestCheck(g, &victim)).To(BeTrue())
				g.Expect(sourceDestCheck(g, &control)).To(BeFalse(), "healthy control %s was not reconciled", control.Name)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for the unsafe victim peer to be removed")
			Eventually(func(g Gomega) {
				peers, listErr := allManagedPeers(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				remainingControlPeers := maps.Clone(oldPeers[controlIP])
				for _, peer := range peers {
					address := aws.ToString(peer.PeerAddress)
					g.Expect(address).NotTo(Equal(victimIP), "victim peer %s remains", aws.ToString(peer.RouteServerPeerId))
					if address == controlIP && peer.State == ec2types.RouteServerPeerStateAvailable {
						delete(remainingControlPeers, aws.ToString(peer.RouteServerPeerId))
					}
				}
				g.Expect(remainingControlPeers).To(BeEmpty(),
					"original healthy control peers were removed: %v", remainingControlPeers)
			}).WithTimeout(peerSettleTimeout).WithPolling(pollInterval).Should(Succeed())

			By("removing the deny and relying on the degraded retry to recover")
			err = deleteInlinePolicy(ctx, op, denyPolicyName)
			Expect(err).NotTo(HaveOccurred(), "cannot remove inline policy from operator %s", op.Kind)
			policyAttached = false

			By("waiting for forwarding, a replacement peer, and Ready status")
			Eventually(func(g Gomega) {
				g.Expect(sourceDestCheck(g, &victim)).To(BeFalse())

				peers, listErr := allManagedPeers(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				replacementAvailable := slices.ContainsFunc(peers, func(peer ec2types.RouteServerPeer) bool {
					return aws.ToString(peer.PeerAddress) == victimIP &&
						!oldPeers[victimIP][aws.ToString(peer.RouteServerPeerId)] &&
						peer.State == ec2types.RouteServerPeerStateAvailable
				})
				g.Expect(replacementAvailable).To(BeTrue())

				fresh := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, fresh)).To(Succeed())
				g.Expect(fresh.Status.Phase).To(Equal(networkingapi.PhaseReady))
			}).WithTimeout(peerSettleTimeout).WithPolling(pollInterval).Should(Succeed())

			By("restoring the original forwarding selector")
			restoredGeneration := restoreForwardingSelector(ctx)
			Eventually(func() error {
				return e2e.CloudPassDone(ctx, k8sClient, bgpConfig.Name, restoredGeneration)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
			selectorChanged = false
		})
	})

	// ---------------------------------------------------------------
	// E2E-AWS-05: Full cleanup lifecycle
	// ---------------------------------------------------------------
	Context("E2E-AWS-05: Full cleanup lifecycle", func() {
		It("should block config deletion while routing CR exists, then clean up everything", func(ctx context.Context) {
			By("reading the config and the CredentialsRequest it should own")
			configCR := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, configCR)).To(Succeed())

			credentialsRequest := &unstructured.Unstructured{}
			credentialsRequest.SetGroupVersionKind(awsplatform.CredentialsRequestGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      awsplatform.CredentialsRequestName,
				Namespace: awsplatform.CredentialsRequestNamespace,
			}, credentialsRequest)
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			// A pod that already holds credentials -- the identity
			// webhook's, or the profile on the desk this is run from --
			// asks the cluster for none, so on such a run there is
			// nothing to own and nothing to collect. Said out loud,
			// because an assertion that passes for want of an object
			// reads exactly like one that passed on its merits.
			askedForCredentials := err == nil
			if askedForCredentials {
				Expect(e2e.CheckOwnedByConfig(credentialsRequest, configCR)).To(Succeed())
			} else {
				GinkgoWriter.Println("no CredentialsRequest: this operator resolved credentials without asking the cluster")
			}

			By("attempting to delete config CR (should be blocked by routing CR)")
			Expect(k8sClient.Delete(ctx, configCR)).To(Succeed())

			By("verifying config CR still exists (finalizer blocks deletion)")
			Consistently(func(g Gomega) {
				cfg := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)).To(Succeed())
				g.Expect(cfg.DeletionTimestamp).NotTo(BeNil(), "should be marked for deletion")
				hasFinalizer := false
				for _, f := range cfg.Finalizers {
					if f == "networking.openshift.io/bgpcloudconfiguration" {
						hasFinalizer = true
					}
				}
				g.Expect(hasFinalizer).To(BeTrue(), "finalizer should still be present")
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("deleting routing CR")
			routingCR := &networkingapi.BGPRouting{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, routingCR)).To(Succeed())
			Expect(k8sClient.Delete(ctx, routingCR)).To(Succeed())

			By("waiting for routing CR to be fully removed")
			Eventually(func(g Gomega) {
				rt := &networkingapi.BGPRouting{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, rt)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "routing CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for config CR to be fully removed")
			Eventually(func(g Gomega) {
				cfg := &networkingapi.BGPCloudConfiguration{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "config CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			if askedForCredentials {
				By("verifying the CredentialsRequest went with the config")
				Eventually(func(g Gomega) {
					cr := &unstructured.Unstructured{}
					cr.SetGroupVersionKind(awsplatform.CredentialsRequestGVK)
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      awsplatform.CredentialsRequestName,
						Namespace: awsplatform.CredentialsRequestNamespace,
					}, cr)
					g.Expect(client.IgnoreNotFound(err)).To(Succeed())
					g.Expect(err).To(HaveOccurred(), "the CredentialsRequest should have been collected with the config")
				}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
			}

			By("verifying external Network/cluster state was not reverted on config delete")
			Expect(e2e.CheckExternalNetworkStatePreserved(ctx, k8sClient)).To(Succeed())

			By("verifying AWS peers are deleted")
			Eventually(func(g Gomega) {
				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(peers).To(BeEmpty(), "all managed peers should be deleted")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying FRRConfigurations are deleted")
			azCount := len(endpointsByAZ)
			for i := 1; i <= azCount; i++ {
				frrCfg := &unstructured.Unstructured{}
				frrCfg.SetGroupVersionKind(schema.GroupVersionKind{
					Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration",
				})
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      frrConfigNamePrefix + itoa(i),
					Namespace: frrNamespace,
				}, frrCfg)
				Expect(client.IgnoreNotFound(err)).To(Succeed())
				Expect(err).To(HaveOccurred(), "FRRConfiguration %d should be deleted", i)
			}
		})
	})
})

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
