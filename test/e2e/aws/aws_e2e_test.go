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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

const (
	frrNamespace           = "openshift-frr-k8s"
	frrConfigNamePrefix    = "cudn-bgp-"
	routeAdvertisementName = "cudn-bgp-route-advertisements"
	labelManagedBy         = "app.kubernetes.io/managed-by"
	labelManagedByVal      = "cudn-bgp-routing-operator"

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
			By("applying CUDNBgpConfig CR")
			configCR := bgpConfig.DeepCopy()
			configCR.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, configCR)).To(Succeed())

			By("waiting for config phase=Ready")
			Eventually(func(g Gomega) {
				cfg := &networkingv1alpha1.CUDNBgpConfig{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
				g.Expect(cfg.Status.Phase).To(Equal(networkingv1alpha1.PhaseReady))
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
			cfgFresh := &networkingv1alpha1.CUDNBgpConfig{}
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

			By("creating labeled namespace for CUDN")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: bgpRouting.Spec.Network.Name,
					Labels: map[string]string{
						"k8s.ovn.org/primary-user-defined-network": "",
						"cluster-udn": bgpRouting.Spec.Network.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			By("applying CUDNBgpRouting CR")
			routingCR := bgpRouting.DeepCopy()
			routingCR.ResourceVersion = ""
			Expect(k8sClient.Create(ctx, routingCR)).To(Succeed())

			By("waiting for routing phase=Ready")
			Eventually(func(g Gomega) {
				rt := &networkingv1alpha1.CUDNBgpRouting{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: routingCR.Name}, rt)).To(Succeed())
				g.Expect(rt.Status.Phase).To(Equal(networkingv1alpha1.PhaseReady))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying routing resources: CUDN, RouteAdvertisements")

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
	// E2E-AWS-05: Full cleanup lifecycle
	// ---------------------------------------------------------------
	Context("E2E-AWS-05: Full cleanup lifecycle", func() {
		It("should block config deletion while routing CR exists, then clean up everything", func(ctx context.Context) {
			By("attempting to delete config CR (should be blocked by routing CR)")
			configCR := &networkingv1alpha1.CUDNBgpConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, configCR)).To(Succeed())
			Expect(k8sClient.Delete(ctx, configCR)).To(Succeed())

			By("verifying config CR still exists (finalizer blocks deletion)")
			Consistently(func(g Gomega) {
				cfg := &networkingv1alpha1.CUDNBgpConfig{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)).To(Succeed())
				g.Expect(cfg.DeletionTimestamp).NotTo(BeNil(), "should be marked for deletion")
				hasFinalizer := false
				for _, f := range cfg.Finalizers {
					if f == "networking.openshift.io/cudnbgpconfig" {
						hasFinalizer = true
					}
				}
				g.Expect(hasFinalizer).To(BeTrue(), "finalizer should still be present")
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("deleting routing CR")
			routingCR := &networkingv1alpha1.CUDNBgpRouting{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, routingCR)).To(Succeed())
			Expect(k8sClient.Delete(ctx, routingCR)).To(Succeed())

			By("waiting for routing CR to be fully removed")
			Eventually(func(g Gomega) {
				rt := &networkingv1alpha1.CUDNBgpRouting{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, rt)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "routing CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for config CR to be fully removed")
			Eventually(func(g Gomega) {
				cfg := &networkingv1alpha1.CUDNBgpConfig{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "config CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying Network/cluster FRR patch was not reverted on config delete")
			Expect(e2e.CheckNetworkFRREnabled(ctx, k8sClient)).To(Succeed())

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
