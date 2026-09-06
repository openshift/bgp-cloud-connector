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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

const (
	reconcileTimeout = 6 * time.Minute
	pollInterval     = 10 * time.Second
)

var _ = Describe("E2E", Ordered, func() {

	// -----------------------------------------------------------
	// E2E-01: Full stack reconcile with BGP session verification
	// -----------------------------------------------------------
	Context("E2E-01: Full stack reconcile", func() {
		It("should apply CRs and reach Ready with established BGP sessions", func(ctx context.Context) {
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
			Expect(CheckConfigNetworkOwnershipExternal(ctx, k8sClient, configCR.Name)).To(Succeed())

			By("verifying FRRConfigurations exist")
			azCount := len(bgpConfig.Spec.BGP.PeerGroups)
			for i := 1; i <= azCount; i++ {
				frrCfg := &unstructured.Unstructured{}
				frrCfg.SetGroupVersionKind(frrConfigurationGVK)
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s%d", frrConfigNamePrefix, i),
					Namespace: frrNamespace,
				}, frrCfg)).To(Succeed(), "FRRConfiguration %d should exist", i)
			}

			By("creating labeled namespace for CUDN")
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "app1",
					Labels: map[string]string{
						labelPrimaryUDN: "",
						labelCUDN:       bgpRouting.Spec.Network.Name,
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

			By("verifying CUDN and RouteAdvertisements exist")
			cudn := &unstructured.Unstructured{}
			cudn.SetGroupVersionKind(cudnGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: "cluster-udn-" + bgpRouting.Spec.Network.Name,
			}, cudn)).To(Succeed())

			ra := &unstructured.Unstructured{}
			ra.SetGroupVersionKind(raGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: raName}, ra)).To(Succeed())

			By("verifying BGP sessions are Established on all router nodes")
			assertBGPEstablished(ctx)

			By("verifying CUDN subnets appear in FRR advertised routes")
			for _, subnet := range bgpRouting.Spec.Network.Subnets {
				assertSubnetAdvertised(ctx, subnet)
			}
		})
	})

	// -----------------------------------------------------------
	// E2E-02: FRRConfiguration deleted — BGP recovery
	// -----------------------------------------------------------
	Context("E2E-02: FRRConfiguration deleted", func() {
		It("should recreate FRRConfiguration and re-establish BGP sessions", func(ctx context.Context) {
			azCount := len(bgpConfig.Spec.BGP.PeerGroups)

			By("deleting operator-managed FRRConfigurations")
			for i := 1; i <= azCount; i++ {
				frrCfg := &unstructured.Unstructured{}
				frrCfg.SetGroupVersionKind(frrConfigurationGVK)
				frrCfg.SetName(fmt.Sprintf("%s%d", frrConfigNamePrefix, i))
				frrCfg.SetNamespace(frrNamespace)
				Expect(k8sClient.Delete(ctx, frrCfg)).To(Succeed())
			}

			By("waiting for operator to recreate FRRConfigurations")
			Eventually(func(g Gomega) {
				for i := 1; i <= azCount; i++ {
					frrCfg := &unstructured.Unstructured{}
					frrCfg.SetGroupVersionKind(frrConfigurationGVK)
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{
						Name:      fmt.Sprintf("%s%d", frrConfigNamePrefix, i),
						Namespace: frrNamespace,
					}, frrCfg)).To(Succeed(), "FRRConfiguration %d should be recreated", i)
				}
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for BGP sessions to re-establish")
			assertBGPEstablished(ctx)
		})
	})

	// -----------------------------------------------------------
	// E2E-03: FRR pod restart — BGP recovery
	// -----------------------------------------------------------
	Context("E2E-03: FRR pod restart", func() {
		It("should re-establish BGP sessions after FRR pods are restarted", func(ctx context.Context) {
			By("deleting all FRR pods")
			frrPods := &corev1.PodList{}
			Expect(k8sClient.List(ctx, frrPods,
				client.InNamespace(frrNamespace),
				client.MatchingLabels{"app": "frr-k8s"},
			)).To(Succeed())
			Expect(frrPods.Items).NotTo(BeEmpty(), "FRR pods should be running")

			for i := range frrPods.Items {
				Expect(k8sClient.Delete(ctx, &frrPods.Items[i])).To(Succeed())
			}

			By("waiting for FRR pods to restart")
			Eventually(func(g Gomega) {
				pods := &corev1.PodList{}
				g.Expect(k8sClient.List(ctx, pods,
					client.InNamespace(frrNamespace),
					client.MatchingLabels{"app": "frr-k8s"},
				)).To(Succeed())
				g.Expect(pods.Items).NotTo(BeEmpty())
				for _, pod := range pods.Items {
					g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
						"FRR pod %s should be Running", pod.Name)
				}
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for BGP sessions to re-establish")
			assertBGPEstablished(ctx)
		})
	})

	// -----------------------------------------------------------
	// E2E-04: Deletion cleanup
	// -----------------------------------------------------------
	Context("E2E-04: Deletion cleanup", func() {
		It("should block config deletion while routing exists, then clean up everything", func(ctx context.Context) {
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

			By("verifying CUDN and RouteAdvertisements are deleted")
			Eventually(func(g Gomega) {
				cudn := &unstructured.Unstructured{}
				cudn.SetGroupVersionKind(cudnGVK)
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: "cluster-udn-" + bgpRouting.Spec.Network.Name,
				}, cudn)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "CUDN should be deleted")

				ra := &unstructured.Unstructured{}
				ra.SetGroupVersionKind(raGVK)
				err = k8sClient.Get(ctx, types.NamespacedName{Name: raName}, ra)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "RouteAdvertisements should be deleted")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("waiting for config CR to be fully removed")
			Eventually(func(g Gomega) {
				cfg := &networkingv1alpha1.CUDNBgpConfig{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg)
				g.Expect(client.IgnoreNotFound(err)).To(Succeed())
				g.Expect(err).To(HaveOccurred(), "config CR should be gone")
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())

			By("verifying Network/cluster FRR patch was not reverted on config delete")
			Expect(CheckNetworkFRREnabled(ctx, k8sClient)).To(Succeed())

			By("verifying FRRConfigurations are deleted")
			azCount := len(bgpConfig.Spec.BGP.PeerGroups)
			for i := 1; i <= azCount; i++ {
				frrCfg := &unstructured.Unstructured{}
				frrCfg.SetGroupVersionKind(frrConfigurationGVK)
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s%d", frrConfigNamePrefix, i),
					Namespace: frrNamespace,
				}, frrCfg)
				Expect(client.IgnoreNotFound(err)).To(Succeed())
				Expect(err).To(HaveOccurred(), "FRRConfiguration %d should be deleted", i)
			}

			By("verifying BGP sessions are no longer Established")
			assertBGPNotEstablished(ctx)

			By("deleting test namespace app1")
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "app1"}, ns)).To(Succeed())
			Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "app1"}, ns)
			}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).ShouldNot(Succeed())
		})
	})
})

func assertBGPNotEstablished(ctx context.Context) {
	neighborAddrs := make(map[string]bool)
	for _, az := range bgpConfig.Spec.BGP.PeerGroups {
		for _, n := range az.Neighbors {
			neighborAddrs[n.Address] = true
		}
	}

	Eventually(func(g Gomega) {
		sessionList := &unstructured.UnstructuredList{}
		sessionList.SetGroupVersionKind(bgpSessionStateGVK)
		g.Expect(k8sClient.List(ctx, sessionList, client.InNamespace(frrNamespace))).To(Succeed())

		for _, s := range sessionList.Items {
			peer, _, _ := unstructured.NestedString(s.Object, "status", "peer")
			if !neighborAddrs[peer] {
				continue
			}
			status, _, _ := unstructured.NestedString(s.Object, "status", "bgpStatus")
			g.Expect(status).NotTo(Equal("Established"),
				"BGP session to %s should not be Established after cleanup", peer)
		}
	}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
}

func assertSubnetAdvertised(ctx context.Context, subnet string) {
	nodes, err := routerNodes(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes).NotTo(BeEmpty())

	Eventually(func(g Gomega) {
		for _, n := range nodes {
			nodeState := &unstructured.Unstructured{}
			nodeState.SetGroupVersionKind(frrNodeStateGVK)
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      n.Name,
				Namespace: frrNamespace,
			}, nodeState)).To(Succeed(), "FRRNodeState for node %s should exist", n.Name)

			runningCfg, _, _ := unstructured.NestedString(nodeState.Object, "status", "runningConfig")
			g.Expect(runningCfg).To(ContainSubstring(subnet),
				"FRR running config on node %s should advertise subnet %s", n.Name, subnet)
		}
	}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
}

func assertBGPEstablished(ctx context.Context) {
	nodes, err := routerNodes(ctx)
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes).NotTo(BeEmpty(), "expected at least 1 router node")

	neighborAddrs := make(map[string]bool)
	for _, az := range bgpConfig.Spec.BGP.PeerGroups {
		for _, n := range az.Neighbors {
			neighborAddrs[n.Address] = true
		}
	}

	Eventually(func(g Gomega) {
		sessionList := &unstructured.UnstructuredList{}
		sessionList.SetGroupVersionKind(bgpSessionStateGVK)
		g.Expect(k8sClient.List(ctx, sessionList, client.InNamespace(frrNamespace))).To(Succeed())

		established := make(map[string]bool)
		for _, s := range sessionList.Items {
			status, _, _ := unstructured.NestedString(s.Object, "status", "bgpStatus")
			peer, _, _ := unstructured.NestedString(s.Object, "status", "peer")
			node, _, _ := unstructured.NestedString(s.Object, "status", "node")
			if status == "Established" && neighborAddrs[peer] {
				established[node] = true
			}
		}

		for _, n := range nodes {
			g.Expect(established).To(HaveKey(n.Name),
				"node %s should have an Established BGP session", n.Name)
		}
	}).WithTimeout(reconcileTimeout).WithPolling(pollInterval).Should(Succeed())
}
