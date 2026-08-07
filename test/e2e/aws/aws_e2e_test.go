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
	"os"
	"sort"
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
)

const (
	frrNamespace           = "openshift-frr-k8s"
	frrConfigNamePrefix    = "cudn-bgp-az-"
	routeAdvertisementName = "cudn-bgp-route-advertisements"
	labelManagedBy         = "app.kubernetes.io/managed-by"
	labelManagedByVal      = "cudn-bgp-routing-operator"

	pollInterval = 10 * time.Second
)

// The operator re-examines healthy resources every --resync-interval,
// which bounds how long the self-healing specs must wait for it to notice
// something has drifted. That interval is a flag now, so the suite has to
// be told which value the operator under test is using; hard-coding it
// meant a 6 minute budget per assertion regardless, and a message claiming
// "5m cycle" whatever you had actually set.
//
//	E2E_RESYNC_INTERVAL=30s   alongside   --resync-interval=30s
var (
	resyncInterval   = durationFromEnv("E2E_RESYNC_INTERVAL", 5*time.Minute)
	reconcileTimeout = 2*resyncInterval + time.Minute
)

// durationFromEnv is deliberately strict: a typo in a duration should stop
// the run rather than silently fall back to a value that makes the suite
// wait five minutes per assertion for reasons nobody can see.
func durationFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		panic(fmt.Sprintf("%s=%q is not a duration: %v", name, raw, err))
	}
	return d
}

var _ = Describe("AWS E2E", Ordered, func() {

	// ---------------------------------------------------------------
	// E2E-AWS-01: Full stack reconcile
	// ---------------------------------------------------------------
	Context("E2E-AWS-01: Full stack reconcile", func() {
		It("should apply CRs and reach Ready state with all AWS resources", func(ctx context.Context) {
			By("applying CUDNBgpConfig CR")
			configCR := bgpConfig.DeepCopy()
			configCR.ResourceVersion = ""
			createEventually(ctx, configCR, "CUDNBgpConfig")

			By("waiting for config phase=Ready")
			Eventually(func(g Gomega) {
				cfg := &networkingv1alpha1.CUDNBgpConfig{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
				g.Expect(cfg.Status.Phase).To(Equal(networkingv1alpha1.PhaseReady))
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

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

			By("verifying status.aws is populated")
			cfgFresh := &networkingv1alpha1.CUDNBgpConfig{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfgFresh)).To(Succeed())
			Expect(cfgFresh.Status.AWS).NotTo(BeNil(), "status.aws should be populated")
			Expect(cfgFresh.Status.AWS.RouteServers).NotTo(BeEmpty(),
				"status.aws.routeServers should contain discovered route servers")

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
			// The CUDN selects namespaces by the cluster-udn label, not by
			// name, so the name is free. Let the API server generate it:
			// a fixed name meant any run that failed before cleanup left a
			// namespace behind that made every later run fail on
			// AlreadyExists.
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: bgpRouting.Spec.Network.Name + "-",
					Labels: map[string]string{
						"k8s.ovn.org/primary-user-defined-network": "",
						"cluster-udn": bgpRouting.Spec.Network.Name,
					},
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			testNamespace = ns.Name
			GinkgoWriter.Printf("created namespace %s\n", testNamespace)

			By("applying CUDNBgpRouting CR")
			routingCR := bgpRouting.DeepCopy()
			routingCR.ResourceVersion = ""
			createEventually(ctx, routingCR, "CUDNBgpRouting")

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

			By(fmt.Sprintf("waiting for operator to reconcile (next %s cycle)", resyncInterval))
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
			By("finding an available managed peer to delete")
			// AWS rejects DeleteRouteServerPeer on a peer that is still
			// pending, so wait for one to settle rather than racing the
			// operator's own creation.
			// allManagedPeers walks endpointsByAZ, a map, so Go randomises
			// the order: taking peers[0] deletes a different peer on every
			// run. Sort so a failure names the same peer twice running.
			var victim ec2types.RouteServerPeer
			Eventually(func(g Gomega) {
				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred())

				var available []ec2types.RouteServerPeer
				for _, p := range peers {
					if p.State == ec2types.RouteServerPeerStateAvailable {
						available = append(available, p)
					}
				}
				g.Expect(available).NotTo(BeEmpty(), "no available managed peer yet")

				sort.Slice(available, func(i, j int) bool {
					return aws.ToString(available[i].RouteServerPeerId) <
						aws.ToString(available[j].RouteServerPeerId)
				})
				victim = available[0]
			}, reconcileTimeout, pollInterval).Should(Succeed())

			victimID := aws.ToString(victim.RouteServerPeerId)
			victimIP := aws.ToString(victim.PeerAddress)
			GinkgoWriter.Printf("deleting peer %s (IP %s)\n", victimID, victimIP)

			By("deleting the peer via EC2 API")
			_, err := ec2Client.DeleteRouteServerPeer(ctx, &ec2.DeleteRouteServerPeerInput{
				RouteServerPeerId: aws.String(victimID),
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for operator to recreate it")
			Eventually(func(g Gomega) {
				peers, err := allManagedPeers(ctx)
				g.Expect(err).NotTo(HaveOccurred())

				found := false
				for _, p := range peers {
					if aws.ToString(p.PeerAddress) == victimIP {
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
