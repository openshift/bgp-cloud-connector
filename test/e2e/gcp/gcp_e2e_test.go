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

package gcp_e2e

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

const (
	frrNamespace           = "openshift-frr-k8s"
	operatorNamespace      = "openshift-bgp-cloud-connector"
	frrConfigNamePrefix    = "bgp-cc-"
	routeAdvertisementName = "bgp-cc-route-advertisements"

	pollInterval = 10 * time.Second

	// The operator writes status once, at the end of a reconcile, so
	// nothing it reports arrives before the cloud work has finished.
	// Measured on 11 September against three router nodes: the spoke
	// alone took about two minutes to reach ACTIVE, and the peers are
	// written in one patch rather than one call each, so GCP is
	// markedly quicker here than Azure.
	reconcileTimeout = 15 * time.Minute

	// Sessions come up only once the spoke is ACTIVE, and the peers are
	// created before it is, so BGP retries for a while after everything
	// else looks finished. Measured: about two minutes from ACTIVE.
	sessionTimeout = 10 * time.Minute

	// Removing the configuration makes the operator delete the peers and
	// the spoke before it clears the finalizer.
	cleanupTimeout = 15 * time.Minute
)

var bgpSessionStateGVK = schema.GroupVersionKind{
	Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "BGPSessionState",
}

var _ = Describe("GCP E2E", Ordered, func() {
	var configCR *networkingapi.BGPCloudConfiguration
	var routingCR *networkingapi.BGPRouting

	// Setup lives here rather than in the first spec so that any spec
	// can be run on its own. Ginkgo runs BeforeAll whatever you focus,
	// so --focus=GCP-03 still gets a cluster with the CRs applied.
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
	// E2E-GCP-01: the operator discovers the estate and peers with it
	// ---------------------------------------------------------------
	Context("E2E-GCP-01: Full stack reconcile", func() {
		It("should report one peer group, two peers per router node, and forwarding enabled", func(ctx context.Context) {
			By("verifying the discovered plan is one group keyed on the Cloud Router")
			cfg := &networkingapi.BGPCloudConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configCR.Name}, cfg)).To(Succeed())
			// A GCP subnet is regional and there is one Cloud Router for
			// the region, so every node peers with the same two
			// interface addresses. That is one group here where AWS has
			// one per availability zone.
			Expect(cfg.Status.PeerGroups).To(HaveLen(1),
				"GCP discovery should report a single peer group: the subnet is regional")
			group := cfg.Status.PeerGroups[0]
			Expect(group.Key).To(Equal(bgpConfig.Spec.GCP.CloudRouterName))
			Expect(group.Neighbors).To(HaveLen(2),
				"a Cloud Router presents its redundant pair of interfaces")

			By("verifying the neighbour ASN is the Cloud Router's, and differs from ours")
			router, err := cloudRouter(ctx)
			Expect(err).NotTo(HaveOccurred())
			for _, n := range group.Neighbors {
				Expect(n.Address).NotTo(BeEmpty())
				Expect(n.RemoteASN).To(BeNumerically("==", router.Bgp.Asn))
				Expect(n.RemoteASN).NotTo(BeNumerically("==", bgpConfig.Spec.BGP.LocalASN),
					"the two ASNs must differ or the session is not eBGP")
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

			By("verifying the router appliance spoke is ACTIVE")
			// Nothing can peer with a Cloud Router until its instances
			// belong to a spoke, and GCP caps a spoke at
			// NCCMaxInstancesPerSpoke instances, so the count is derived
			// rather than fixed at one.
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty(), "expected at least one router node")
			wantSpokes := (len(nodes) + gcpplatform.NCCMaxInstancesPerSpoke - 1) / gcpplatform.NCCMaxInstancesPerSpoke

			spokes, err := hubSpokes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(spokes).To(HaveLen(wantSpokes))
			for _, s := range spokes {
				Expect(s.State).To(Equal("ACTIVE"), "spoke %s is %s", s.Name, s.State)
			}

			By("verifying GCP has two peers per router node, one per interface")
			peers, err := managedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Two interfaces, so two peers per node: a redundant pair,
			// not the one-per-node Azure has.
			Expect(peers).To(HaveLen(len(nodes)*2),
				"one peer per node per Cloud Router interface")

			byName := make(map[string]*compute.RouterBgpPeer, len(peers))
			for _, p := range peers {
				byName[p.Name] = p
			}
			for i := range nodes {
				ip := nodeInternalIP(&nodes[i])
				Expect(ip).NotTo(BeEmpty(), "node %s has no internal address", nodes[i].Name)
				for iface := 0; iface < 2; iface++ {
					// PeerName rather than a reimplementation, so the
					// suite and the operator cannot drift on naming.
					name := gcpplatform.PeerName(clusterID, ip, iface)
					p, ok := byName[name]
					Expect(ok).To(BeTrue(), "no peer %s for node %s", name, nodes[i].Name)
					Expect(p.PeerIpAddress).To(Equal(ip))
					Expect(p.PeerAsn).To(BeNumerically("==", bgpConfig.Spec.BGP.LocalASN))
				}
			}

			By("verifying canIpForward is on for every router node")
			for i := range nodes {
				inst, instErr := instanceFor(ctx, &nodes[i])
				Expect(instErr).NotTo(HaveOccurred())
				Expect(inst.CanIpForward).To(BeTrue(),
					"node %s would drop forwarded packets, and every condition would still be True", nodes[i].Name)
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
	// E2E-GCP-02: a peer removed behind the operator's back
	// ---------------------------------------------------------------
	Context("E2E-GCP-02: Cloud Router peer deleted", func() {
		It("should rebuild the peer and re-establish the session", func(ctx context.Context) {
			peers, err := managedPeers(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(peers).NotTo(BeEmpty())
			victim := peers[0].Name

			By("removing peer " + victim + " via the GCP API")
			Expect(removePeer(ctx, victim)).To(Succeed())

			Eventually(func(g Gomega) {
				current, listErr := managedPeers(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				for _, p := range current {
					g.Expect(p.Name).NotTo(Equal(victim))
				}
			}).WithTimeout(3*time.Minute).WithPolling(pollInterval).Should(Succeed(),
				"the peer should be gone before we watch it come back")

			By("waiting for the operator to rebuild it")
			Eventually(func(g Gomega) {
				current, listErr := managedPeers(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				found := false
				for _, p := range current {
					if p.Name == victim {
						found = true
					}
				}
				g.Expect(found).To(BeTrue(), "peer %s was not rebuilt", victim)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for the session to re-establish")
			assertBGPEstablished(ctx)
		})
	})

	// ---------------------------------------------------------------
	// E2E-GCP-03: forwarding turned off behind the operator's back
	// ---------------------------------------------------------------
	Context("E2E-GCP-03: canIpForward disabled on a router node", func() {
		It("should enable it again", func(ctx context.Context) {
			nodes, err := routerNodes(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes).NotTo(BeEmpty())
			victim := nodes[0]

			By("disabling canIpForward on " + victim.Name)
			Expect(setCanIPForward(ctx, &victim, false)).To(Succeed())

			By("waiting for the operator to put it back")
			// This is the failure that looks healthy: with forwarding
			// off BGP still establishes and every condition stays True
			// while no packet reaches a pod.
			Eventually(func(g Gomega) {
				inst, instErr := instanceFor(ctx, &victim)
				g.Expect(instErr).NotTo(HaveOccurred())
				g.Expect(inst.CanIpForward).To(BeTrue())
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
		})
	})

	// ---------------------------------------------------------------
	// E2E-GCP-04: deletion order and cleanup
	// ---------------------------------------------------------------
	Context("E2E-GCP-04: Deletion cleanup", func() {
		It("should block config deletion while routing exists, then remove peers and spoke", func(ctx context.Context) {
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
			}).WithTimeout(cleanupTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying every peer this cluster owned has gone from the Cloud Router")
			Eventually(func(g Gomega) {
				current, listErr := managedPeers(ctx)
				g.Expect(listErr).NotTo(HaveOccurred())
				g.Expect(current).To(BeEmpty())
			}).WithTimeout(cleanupTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying the spoke has gone, so the hub can be deleted")
			// The hub refuses while a spoke is attached, so a spoke left
			// behind is what makes a teardown leak the hub.
			Eventually(func(g Gomega) {
				spokes, spokeErr := hubSpokes(ctx)
				g.Expect(spokeErr).NotTo(HaveOccurred())
				g.Expect(spokes).To(BeEmpty())
			}).WithTimeout(cleanupTimeout).WithPolling(pollInterval).Should(Succeed())

			By("deleting the test namespace")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bgpRouting.Spec.Network.Name}}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
		})
	})
})

// removePeer takes one BGP peer off the Cloud Router behind the
// operator's back, so a spec can watch it be rebuilt.
//
// A patch replaces the whole peer list, so the remaining peers are sent
// back unchanged rather than the one being removed sent as a deletion:
// that is how the API expresses it, and it is why the operator names
// peers rather than numbering them.
func removePeer(ctx context.Context, name string) error {
	router, err := cloudRouter(ctx)
	if err != nil {
		return err
	}
	kept := make([]*compute.RouterBgpPeer, 0, len(router.BgpPeers))
	for _, p := range router.BgpPeers {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	patch := &compute.Router{BgpPeers: kept, ForceSendFields: []string{"BgpPeers"}}
	var op *compute.Operation
	if err := retryTransient(func() error {
		var callErr error
		op, callErr = computeSvc.Routers.Patch(
			bgpConfig.Spec.GCP.Project, bgpConfig.Spec.GCP.Region,
			bgpConfig.Spec.GCP.CloudRouterName, patch).Context(ctx).Do()
		return callErr
	}); err != nil {
		return err
	}
	return waitRegionOp(ctx, op)
}

// setCanIPForward rewrites the flag on a node's instance, so a spec can
// turn it off and watch the operator put it back.
func setCanIPForward(ctx context.Context, node *corev1.Node, enabled bool) error {
	vm, err := gcpplatform.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return err
	}
	inst, err := computeSvc.Instances.Get(vm.Project, vm.Zone, vm.Name).Context(ctx).Do()
	if err != nil {
		return err
	}
	update := &compute.Instance{
		Name:            inst.Name,
		CanIpForward:    enabled,
		Fingerprint:     inst.Fingerprint,
		ForceSendFields: []string{"CanIpForward"},
	}
	var op *compute.Operation
	if err := retryTransient(func() error {
		var callErr error
		op, callErr = computeSvc.Instances.Update(vm.Project, vm.Zone, vm.Name, update).Context(ctx).Do()
		return callErr
	}); err != nil {
		return err
	}
	return waitZoneOp(ctx, vm.Zone, op)
}

// retryTransient retries a call GCP refused for its own reasons.
//
// These helpers are how a spec perturbs the estate so it can watch the
// operator repair it; they are setup, not the thing under test. Failing
// a spec because Google returned a 503 tells you nothing about the
// operator and makes the suite flaky. Observed: "Error 503: Internal
// error. Please try again", on an Instances.Update, with the cluster
// entirely healthy either side of it.
//
// Only server-side refusals are retried. A 4xx means the request was
// wrong, which is a real failure and should stay one.
func retryTransient(call func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = call(); err == nil {
			return nil
		}
		var apiErr *googleapi.Error
		if !errors.As(err, &apiErr) || apiErr.Code < 500 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
	}
	return err
}

func waitRegionOp(ctx context.Context, op *compute.Operation) error {
	return waitOp(ctx, func() (*compute.Operation, error) {
		return computeSvc.RegionOperations.Get(
			bgpConfig.Spec.GCP.Project, bgpConfig.Spec.GCP.Region, op.Name).Context(ctx).Do()
	})
}

func waitZoneOp(ctx context.Context, zone string, op *compute.Operation) error {
	return waitOp(ctx, func() (*compute.Operation, error) {
		return computeSvc.ZoneOperations.Get(
			bgpConfig.Spec.GCP.Project, zone, op.Name).Context(ctx).Do()
	})
}

func waitOp(ctx context.Context, get func() (*compute.Operation, error)) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		op, err := get()
		if err != nil {
			return err
		}
		if op.Status == "DONE" {
			if op.Error != nil && len(op.Error.Errors) > 0 {
				return fmt.Errorf("operation failed: %s", op.Error.Errors[0].Message)
			}
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("operation did not finish within five minutes")
}

// assertBGPEstablished waits until every router node has a session in
// Established to one of the addresses the operator discovered.
//
// The neighbours come from status.peerGroups rather than from the spec:
// the CRD forbids spec.bgp.peerGroups on a cloud platform, so on GCP the
// spec has none to read.
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
				"node %s has no Established session to the Cloud Router", n.Name)
		}
	}).WithTimeout(sessionTimeout).WithPolling(pollInterval).Should(Succeed())
}
