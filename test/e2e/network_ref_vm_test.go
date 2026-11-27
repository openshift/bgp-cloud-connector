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
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

const vmReferenceOptInLabel = "networking.openshift.io/bgp-network-ref"

// TestNetworkRefVM uses existing BGP peering and an external client, as in
// the Red Hat OSD/GCP guide. It is independent of the shared suite, which
// creates and tears down its own manual peering configuration.
func TestNetworkRefVM(t *testing.T) {
	runner := os.Getenv("E2E_VM_EXTERNAL_EXEC")
	if runner == "" {
		t.Skip("set E2E_VM_EXTERNAL_EXEC to run commands on the external client")
	}

	g := NewWithT(t)
	ctx := t.Context()
	_, err := exec.LookPath(runner)
	g.Expect(err).NotTo(HaveOccurred())
	_, err = exec.LookPath("oc")
	g.Expect(err).NotTo(HaveOccurred())

	cidr, err := netip.ParsePrefix(os.Getenv("E2E_VM_CIDR"))
	g.Expect(err).NotTo(HaveOccurred(), "set E2E_VM_CIDR to an unused IPv4 CUDN subnet")
	g.Expect(cidr.Addr().Is4() && cidr == cidr.Masked()).To(BeTrue(), "E2E_VM_CIDR must be a canonical IPv4 subnet")
	node := os.Getenv("E2E_VM_ROUTER_NODE")
	g.Expect(node).NotTo(BeEmpty(), "set E2E_VM_ROUTER_NODE to a peered, virtualization-capable worker")
	image := os.Getenv("E2E_VM_IMAGE")
	if image == "" {
		image = "quay.io/containerdisks/fedora:latest"
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	g.Expect(err).NotTo(HaveOccurred())
	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(networkingapi.AddToScheme(scheme)).To(Succeed())
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())

	config := &networkingapi.BGPCloudConfiguration{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: "cluster"}, config)).To(Succeed())
	g.Expect(config.Status.Phase).To(Equal(networkingapi.PhaseReady), "existing BGPCloudConfiguration must be ready")
	speaker := &corev1.Node{}
	g.Expect(c.Get(ctx, client.ObjectKey{Name: node}, speaker)).To(Succeed())
	kvm := speaker.Status.Allocatable["devices.kubevirt.io/kvm"]
	g.Expect(kvm.Sign()).To(Equal(1), "router node %s must be virtualization-capable", node)
	hostname := speaker.Labels[corev1.LabelHostname]
	g.Expect(hostname).NotTo(BeEmpty())
	peers, err := vmEstablishedPeers(ctx, c, config, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(peers).NotTo(BeEmpty(), "expected an established connector-managed IPv4 peer on %s", node)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: "networkref-vm-", Labels: map[string]string{labelPrimaryUDN: ""},
	}}
	createVMTestObject(t, c, ns)
	cudn := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"namespaceSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": ns.Name},
			},
			"network": map[string]interface{}{
				"topology": "Layer2",
				"layer2": map[string]interface{}{
					"role": "Primary", "subnets": []interface{}{cidr.String()},
					"ipam": map[string]interface{}{"lifecycle": "Persistent"},
				},
			},
		},
	}}
	cudn.SetGroupVersionKind(clusterUDNGVK)
	cudn.SetName(ns.Name)
	createVMTestObject(t, c, cudn)
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition",
	})
	g.Eventually(func() error {
		return c.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: cudn.GetName()}, nad)
	}, reconcileTimeout, pollInterval).Should(Succeed())

	t.Log("starting the VM on its primary CUDN before creating BGPRouting")
	vm := &unstructured.Unstructured{}
	g.Expect(yaml.Unmarshal([]byte(fmt.Sprintf(referenceVMManifest, ns.Name, hostname, image)), &vm.Object)).To(Succeed())
	createVMTestObject(t, c, vm)
	probe := vmInternalClient(ns.Name, hostname)
	createVMTestObject(t, c, probe)

	var ip netip.Addr
	var instanceUID, bootID string
	g.Eventually(func(g Gomega) {
		vmi, address, err := runningVMAddress(ctx, c, client.ObjectKeyFromObject(vm), cidr)
		g.Expect(err).NotTo(HaveOccurred())
		vmNode, _, _ := unstructured.NestedString(vmi.Object, "status", "nodeName")
		g.Expect(vmNode).To(Equal(node))
		output, err := vmInternalHTTP(ctx, probe, address)
		g.Expect(err).NotTo(HaveOccurred())
		_, err = uuid.Parse(output)
		g.Expect(err).NotTo(HaveOccurred(), "the endpoint must return the guest boot ID")
		ip, instanceUID, bootID = address, string(vmi.GetUID()), output
	}, 10*time.Minute, pollInterval).Should(Succeed())

	checkExternal := func(g Gomega, reachable bool) {
		output, code, err := vmExternalHTTP(ctx, runner, ip)
		g.Expect(err).NotTo(HaveOccurred())
		if reachable {
			g.Expect(code).To(Equal("0"), "external HTTP probe should succeed")
			g.Expect(output).To(Equal(bootID))
			return
		}
		g.Expect(code).To(BeElementOf("7", "28"), "expected a connection failure or timeout from the external client")
	}
	g.Expect(vmPrefixExport(ctx, c, node, peers, cidr, false)).To(Succeed())
	checkExternal(g, false)

	t.Log("advertising the existing network and reaching the same running VM from the external client")
	before := cudn.DeepCopy()
	cudn.SetLabels(map[string]string{vmReferenceOptInLabel: string(cudn.GetUID())})
	g.Expect(c.Patch(ctx, cudn, client.MergeFrom(before))).To(Succeed())
	routing := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: ns.Name},
		Spec: networkingapi.BGPRoutingSpec{
			NetworkRef: &networkingapi.RoutingNetworkReference{Kind: clusterUDNGVK.Kind, Name: cudn.GetName()},
		},
	}
	createVMTestObject(t, c, routing)
	g.Eventually(func(g Gomega) {
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(routing), routing)).To(Succeed())
		g.Expect(routing.Status.Phase).To(Equal(networkingapi.PhaseReady), "conditions: %+v", routing.Status.Conditions)
	}, reconcileTimeout, pollInterval).Should(Succeed())
	g.Expect(routing.Status.RouteAdvertisementsRef).NotTo(BeNil())
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(raGVK)
	ra.SetName(routing.Status.RouteAdvertisementsRef.Name)
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ra), ra)).To(Succeed())
	g.Eventually(func() error {
		return vmPrefixExport(ctx, c, node, peers, cidr, true)
	}, reconcileTimeout, pollInterval).Should(Succeed())
	g.Eventually(func(g Gomega) { checkExternal(g, true) }, reconcileTimeout, pollInterval).Should(Succeed())

	t.Log("deleting BGPRouting and verifying withdrawal while the original VM stays reachable inside the CUDN")
	g.Expect(c.Delete(ctx, routing)).To(Succeed())
	g.Eventually(func(g Gomega) {
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(ra), ra)).To(Satisfy(apierrors.IsNotFound))
		g.Expect(c.Get(ctx, client.ObjectKeyFromObject(routing), routing)).To(Satisfy(apierrors.IsNotFound))
	}, reconcileTimeout, pollInterval).Should(Succeed())
	g.Eventually(func(g Gomega) {
		established, err := vmEstablishedPeers(ctx, c, config, node)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(established).To(ContainElements(peers), "the original peers must remain established")
		g.Expect(vmPrefixExport(ctx, c, node, peers, cidr, false)).To(Succeed())
	}, reconcileTimeout, pollInterval).Should(Succeed())
	g.Eventually(func(g Gomega) { checkExternal(g, false) }, reconcileTimeout, pollInterval).Should(Succeed())

	cudnUID := cudn.GetUID()
	g.Expect(c.Get(ctx, client.ObjectKeyFromObject(cudn), cudn)).To(Succeed())
	g.Expect(cudn.GetUID()).To(Equal(cudnUID))
	vmi, address, err := runningVMAddress(ctx, c, client.ObjectKeyFromObject(vm), cidr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(vmi.GetUID())).To(Equal(instanceUID))
	g.Expect(address).To(Equal(ip))
	inside, err := vmInternalHTTP(ctx, probe, ip)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(inside).To(Equal(bootID), "the guest must not have rebooted")
}

func createVMTestObject(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(t.Context(), obj); err != nil {
		t.Fatalf("creating %T %s: %v", obj, client.ObjectKeyFromObject(obj), err)
	}
	uid := obj.GetUID()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), reconcileTimeout)
		defer cancel()
		if err := client.IgnoreNotFound(c.Delete(ctx, obj, client.Preconditions{UID: &uid})); err != nil {
			t.Errorf("deleting %s: %v", client.ObjectKeyFromObject(obj), err)
		}
	})
}

func runningVMAddress(ctx context.Context, c client.Client, key client.ObjectKey,
	cidr netip.Prefix) (*unstructured.Unstructured, netip.Addr, error) {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"})
	if err := c.Get(ctx, key, vmi); err != nil {
		return nil, netip.Addr{}, err
	}
	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	if phase != "Running" {
		return nil, netip.Addr{}, fmt.Errorf("VMI %s is %q, waiting for Running", key, phase)
	}
	interfaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	for _, entry := range interfaces {
		if iface, ok := entry.(map[string]interface{}); ok {
			raw, _, _ := unstructured.NestedString(iface, "ipAddress")
			if ip, err := netip.ParseAddr(raw); err == nil && cidr.Contains(ip) {
				return vmi, ip, nil
			}
		}
	}
	return nil, netip.Addr{}, fmt.Errorf("VMI %s has no guest address in %s yet", key, cidr)
}

func vmInternalClient(namespace, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "internal-probe", Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{corev1.LabelHostname: node},
			Containers: []corev1.Container{{
				Name: "probe", Image: "registry.access.redhat.com/ubi9/ubi-minimal", Command: []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr.To(false), RunAsNonRoot: ptr.To(true),
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
}

func vmInternalHTTP(ctx context.Context, pod *corev1.Pod, ip netip.Addr) (string, error) {
	output, err := oc(ctx, "-n", pod.Namespace, "exec", pod.Name, "--",
		"curl", "--noproxy", "*", "-fsS", "--max-time", "5", vmHTTPURL(ip))
	return strings.TrimSpace(string(output)), err
}

func vmExternalHTTP(ctx context.Context, runner string, ip netip.Addr) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A remote curl failure must be distinguishable from an SSH or runner failure.
	const marker = "\nnetworkref-curl-exit="
	script := fmt.Sprintf(`code=0; curl --noproxy '*' -fsS --max-time 5 '%s' || code=$?;
printf '\nnetworkref-curl-exit=%%s\n' "$code"`, vmHTTPURL(ip))
	output, err := exec.CommandContext(ctx, runner, script).Output()
	if err != nil {
		return "", "", fmt.Errorf("running the external client: %w", err)
	}
	body, code, found := strings.Cut(string(output), marker)
	if !found {
		return "", "", fmt.Errorf("external client did not report curl's exit code: %q", output)
	}
	return strings.TrimSpace(body), strings.TrimSpace(code), nil
}

func vmHTTPURL(ip netip.Addr) string {
	return "http://" + net.JoinHostPort(ip.String(), "8080") + "/"
}

func vmEstablishedPeers(ctx context.Context, c client.Client,
	config *networkingapi.BGPCloudConfiguration, node string) ([]string, error) {
	neighbors := map[string]bool{}
	for _, group := range config.Status.PeerGroups {
		for _, n := range group.Neighbors {
			neighbors[n.Address] = true
		}
	}
	for _, group := range config.Spec.BGP.PeerGroups {
		for _, n := range group.Neighbors {
			neighbors[n.Address] = true
		}
	}
	sessions := &unstructured.UnstructuredList{}
	sessions.SetGroupVersionKind(bgpSessionStateGVK.GroupVersion().WithKind("BGPSessionStateList"))
	if err := c.List(ctx, sessions, client.InNamespace(frrNamespace)); err != nil {
		return nil, err
	}
	var peers []string
	for _, session := range sessions.Items {
		peer, _, _ := unstructured.NestedString(session.Object, "status", "peer")
		owner, _, _ := unstructured.NestedString(session.Object, "status", "node")
		state, _, _ := unstructured.NestedString(session.Object, "status", "bgpStatus")
		ip, err := netip.ParseAddr(peer)
		if err == nil && ip.Is4() && neighbors[peer] && owner == node && state == "Established" {
			peers = append(peers, peer)
		}
	}
	return peers, nil
}

func vmPrefixExport(ctx context.Context, c client.Client, node string, peers []string,
	cidr netip.Prefix, wantAdvertised bool) error {
	pods, err := frrPods(ctx, c)
	if err != nil {
		return err
	}
	if pods[node] == "" {
		return fmt.Errorf("no FRR pod on %s", node)
	}
	for _, peer := range peers {
		output, err := oc(ctx, "-n", frrNamespace, "exec", pods[node], "-c", "frr", "--",
			"vtysh", "-c", fmt.Sprintf("show bgp ipv4 unicast neighbors %s advertised-routes json", peer))
		if err != nil {
			return err
		}
		var routes struct {
			AdvertisedRoutes   map[string]json.RawMessage `json:"advertisedRoutes"`
			TotalPrefixCounter *int                       `json:"totalPrefixCounter"`
		}
		if err := json.Unmarshal(output, &routes); err != nil || routes.TotalPrefixCounter == nil {
			return fmt.Errorf("unexpected FRR advertised-routes output for peer %s: %s", peer, output)
		}
		_, advertised := routes.AdvertisedRoutes[cidr.String()]
		if advertised != wantAdvertised {
			return fmt.Errorf("prefix %s advertised to %s on %s is %t, want %t", cidr, peer, node, advertised, wantAdvertised)
		}
	}
	return nil
}

const referenceVMManifest = `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: guest
  namespace: %q
spec:
  runStrategy: Always
  template:
    spec:
      nodeSelector:
        kubernetes.io/hostname: %q
      domain:
        cpu:
          cores: 2
        memory:
          guest: 2Gi
        devices:
          disks:
            - name: root
              disk:
                bus: virtio
            - name: cloudinit
              disk:
                bus: virtio
          interfaces:
            - name: default
              bridge: {}
      networks:
        - name: default
          pod: {}
      volumes:
        - name: root
          containerDisk:
            image: %q
        - name: cloudinit
          cloudInitNoCloud:
            userData: |
              #cloud-config
              write_files:
                - path: /etc/systemd/system/networkref-probe.service
                  permissions: '0644'
                  content: |
                    [Unit]
                    After=network-online.target
                    Wants=network-online.target
                    [Service]
                    RuntimeDirectory=networkref-probe
                    ExecStartPre=/usr/bin/cp /proc/sys/kernel/random/boot_id /run/networkref-probe/index.html
                    ExecStart=/usr/bin/python3 -m http.server 8080 --directory /run/networkref-probe
                    Restart=on-failure
                    [Install]
                    WantedBy=multi-user.target
              runcmd:
                - [systemctl, daemon-reload]
                - [systemctl, enable, --now, networkref-probe.service]
`
