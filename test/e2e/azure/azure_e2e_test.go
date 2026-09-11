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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

const (
	frrNamespace           = "openshift-frr-k8s"
	frrConfigNamePrefix    = "bgp-cc-"
	routeAdvertisementName = "bgp-cc-route-advertisements"

	pollInterval = 10 * time.Second

	// The operator writes status once, at the end of a reconcile, so
	// nothing it reports arrives before the cloud work has also
	// finished. Measured on 10 September against three router nodes:
	// discovery a second after the apply, three peerings at 3m40s,
	// 2m21s and 2m24s, and the configuration Ready 8m50s in.
	reconcileTimeout = 20 * time.Minute

	// One write at a time on a Route Server, so a peering the operator
	// has to rebuild waits behind anything already in flight.
	peeringSettleTimeout = 15 * time.Minute

	// Removing the configuration makes the operator delete every
	// peering before it clears the finalizer, serially, minutes each.
	cleanupTimeout = 20 * time.Minute
)

var bgpSessionStateGVK = schema.GroupVersionKind{
	Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "BGPSessionState",
}

var _ = Describe("Azure E2E", Ordered, func() {
	var configCR *networkingapi.BGPCloudConfiguration
	var routingCR *networkingapi.BGPRouting

	// Setup lives here rather than in the first spec so that any spec
	// can be run on its own. Ginkgo runs BeforeAll whatever you focus,
	// so `--focus=AZURE-03` still gets a cluster with the CRs applied,
	// which is how you debug one assertion without waiting out the
	// others.
	BeforeAll(func(ctx context.Context) {
		By("applying BGPCloudConfiguration CR")
		configCR = bgpConfig.DeepCopy()
		configCR.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, configCR)).To(Succeed())

		By("waiting for config phase=Ready")
		Eventually(func(g Gomega) {
			cfg := &networkingapi.BGPCloudConfiguration{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			g.Expect(cfg.Status.Phase).To(Equal(networkingapi.PhaseReady))
		}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

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
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())

		By("applying BGPRouting CR")
		routingCR = bgpRouting.DeepCopy()
		routingCR.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, routingCR)).To(Succeed())

		By("waiting for routing phase=Ready")
		Eventually(func(g Gomega) {
			rt := &networkingapi.BGPRouting{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: routingCR.Name}, rt)).To(Succeed())
			g.Expect(rt.Status.Phase).To(Equal(networkingapi.PhaseReady))
		}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-01: the operator discovered the estate and peered with it
	// ---------------------------------------------------------------
	Context("E2E-AZURE-01: Full stack reconcile", func() {
		It("should report one peer group, a peering per router node and forwarding enabled", func(ctx context.Context) {
			By("verifying the discovered peering plan is one group keyed on the Route Server")
			cfg := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			// Azure allows one Route Server per virtual network and
			// presents a redundant pair of addresses for the whole
			// vnet, so there is exactly one group here where AWS has
			// one per availability zone.
			Expect(cfg.Status.PeerGroups).To(HaveLen(1),
				"Azure discovery should report a single peer group")
			group := cfg.Status.PeerGroups[0]
			Expect(group.Key).To(Equal(bgpConfig.Spec.Azure.RouteServerName))
			Expect(group.Neighbors).To(HaveLen(2),
				"a Route Server presents two virtualRouterIps")
			for _, n := range group.Neighbors {
				Expect(n.Address).NotTo(BeEmpty())
				// A Route Server's ASN is fixed by Azure at 65515 and
				// the node addresses are not in its subnet, so every
				// neighbour is multi-hop external BGP.
				Expect(n.RemoteASN).To(BeNumerically("==", 65515))
				Expect(n.EBGPMultiHop).To(BeTrue())
			}

			By("verifying one FRRConfiguration exists")
			frrCfg := &unstructured.Unstructured{}
			frrCfg.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration",
			})
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s%d", frrConfigNamePrefix, 1),
				Namespace: frrNamespace,
			}, frrCfg)).To(Succeed())

			By("verifying Azure has one peering per router node, at the cluster's ASN")
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty(), "expected at least one router node")

			peerings, err := managedPeerings(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peerings).To(HaveLen(len(nodes)),
				"one peering per router node: Azure peers the nodes, not the zones")

			byIP := make(map[string]observedPeering, len(peerings))
			for _, p := range peerings {
				byIP[p.PeerIP] = p
			}
			for _, n := range nodes {
				ip := nodeInternalIP(&n)
				Expect(ip).NotTo(BeEmpty(), "node %s has no internal address", n.Name)
				p, ok := byIP[ip]
				Expect(ok).To(BeTrue(), "no Route Server peering for node %s at %s", n.Name, ip)
				Expect(p.PeerASN).To(BeNumerically("==", bgpConfig.Spec.BGP.LocalASN))
				// A peering Azure failed to apply keeps its name, IP
				// and ASN, so the state is the only thing that says it
				// is really there.
				Expect(p.ProvisioningState).To(Equal("Succeeded"),
					"peering %s for node %s is %s", p.Name, n.Name, p.ProvisioningState)
			}

			By("verifying IP forwarding is enabled on every router node's interface")
			for _, n := range nodes {
				nic, nicErr := nicForNode(ctx, &n)
				Expect(nicErr).NotTo(HaveOccurred())
				Expect(nic).NotTo(BeNil(), "no interface found for node %s", n.Name)
				Expect(nic.Properties.EnableIPForwarding).NotTo(BeNil())
				Expect(*nic.Properties.EnableIPForwarding).To(BeTrue(),
					"node %s would drop forwarded packets, and every condition would still be True", n.Name)
			}

			By("verifying ClusterUDN and RouteAdvertisements exist")
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

			By("verifying every router node has an Established BGP session")
			assertBGPEstablished(ctx)
		})
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-02: a peering removed behind the operator's back
	// ---------------------------------------------------------------
	Context("E2E-AZURE-02: Route Server peering deleted", func() {
		It("should rebuild the peering and re-establish the session", func(ctx context.Context) {
			peerings, err := managedPeerings(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peerings).NotTo(BeEmpty())
			victim := peerings[0]

			By("deleting peering " + victim.Name + " via the Azure API")
			Expect(deletePeering(ctx, victim.Name)).To(Succeed())

			Eventually(func(g Gomega) {
				current, listErr := managedPeerings(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				for _, p := range current {
					g.Expect(p.Name).NotTo(Equal(victim.Name))
				}
			}).WithTimeout(2*time.Minute).WithPolling(pollInterval).Should(Succeed(),
				"the peering should be gone before we watch it come back")

			By("waiting for the operator to rebuild it")
			Eventually(func(g Gomega) {
				current, listErr := managedPeerings(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				found := false
				for _, p := range current {
					if p.Name == victim.Name {
						found = true
						g.Expect(p.PeerIP).To(Equal(victim.PeerIP))
						g.Expect(p.ProvisioningState).To(Equal("Succeeded"))
					}
				}
				g.Expect(found).To(BeTrue(), "peering %s was not rebuilt", victim.Name)
			}).WithTimeout(peeringSettleTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for the session to re-establish")
			assertBGPEstablished(ctx)
		})
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-03: forwarding turned off behind the operator's back
	// ---------------------------------------------------------------
	Context("E2E-AZURE-03: IP forwarding disabled on a router node", func() {
		It("should enable it again", func(ctx context.Context) {
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())
			victim := nodes[0]

			nic, err := nicForNode(ctx, &victim)
			Expect(err).NotTo(HaveOccurred())
			Expect(nic).NotTo(BeNil())

			By("disabling enableIPForwarding on " + victim.Name)
			Expect(setForwarding(ctx, nic, false)).To(Succeed())

			By("waiting for the operator to put it back")
			// This is the failure that looks healthy: with forwarding
			// off BGP still establishes and every condition stays True
			// while no packet reaches a pod.
			Eventually(func(g Gomega) {
				current, nicErr := nicForNode(ctx, &victim)
				g.Expect(nicErr).NotTo(HaveOccurred())
				g.Expect(current).NotTo(BeNil())
				g.Expect(current.Properties.EnableIPForwarding).NotTo(BeNil())
				g.Expect(*current.Properties.EnableIPForwarding).To(BeTrue())
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-AZURE-04: deletion order and cleanup
	// ---------------------------------------------------------------
	Context("E2E-AZURE-04: Deletion cleanup", func() {
		It("should block config deletion while routing exists, then remove every peering", func(ctx context.Context) {
			By("attempting to delete the config CR while the routing CR still exists")
			Expect(k8sClient.Delete(ctx, configCR)).To(Succeed())

			By("verifying the config CR survives, held by its finalizer")
			Consistently(func(g Gomega) {
				cfg := &networkingapi.BGPCloudConfiguration{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			}).WithTimeout(30 * time.Second).WithPolling(5 * time.Second).Should(Succeed())

			By("deleting the routing CR")
			Expect(k8sClient.Delete(ctx, routingCR)).To(Succeed())
			Eventually(func(g Gomega) {
				rt := &networkingapi.BGPRouting{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: routingCR.Name}, rt)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "routing CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for the config CR to be removed")
			Eventually(func(g Gomega) {
				cfg := &networkingapi.BGPCloudConfiguration{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "config CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying every peering this cluster owned has gone from the Route Server")
			Eventually(func(g Gomega) {
				current, listErr := managedPeerings(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				g.Expect(current).To(BeEmpty())
			}).WithTimeout(peeringSettleTimeout).WithPolling(pollInterval).Should(Succeed())

			By("deleting the test namespace")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bgpRouting.Spec.Network.Name}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
		})
	})
})

// assertBGPEstablished waits until every router node has a session in
// Established to one of the addresses the operator discovered.
//
// The neighbours come from status.peerGroups rather than from the spec:
// the CRD forbids spec.bgp.peerGroups on a cloud platform, so on Azure
// the spec has none to read.
func assertBGPEstablished(ctx context.Context) {
	nodes, err := routerNodes(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes).NotTo(BeEmpty())

	cfg := &networkingapi.BGPCloudConfiguration{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)).To(Succeed())
	neighbourAddrs := make(map[string]bool)
	for _, group := range cfg.Status.PeerGroups {
		for _, n := range group.Neighbors {
			neighbourAddrs[n.Address] = true
		}
	}
	Expect(neighbourAddrs).NotTo(BeEmpty(), "status.peerGroups named no neighbours")

	Eventually(func(g Gomega) {
		sessions := &unstructured.UnstructuredList{}
		sessions.SetGroupVersionKind(bgpSessionStateGVK)
		g.Expect(k8sClient.List(ctx, sessions, client.InNamespace(frrNamespace))).To(Succeed())

		established := make(map[string]bool)
		for _, s := range sessions.Items {
			status, _, _ := unstructured.NestedString(s.Object, "status", "bgpStatus")
			peer, _, _ := unstructured.NestedString(s.Object, "status", "peer")
			node, _, _ := unstructured.NestedString(s.Object, "status", "node")
			if status == "Established" && neighbourAddrs[peer] {
				established[node] = true
			}
		}
		for _, n := range nodes {
			g.Expect(established).To(HaveKey(n.Name),
				"node %s has no Established session to the Route Server", n.Name)
		}
	}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
}
