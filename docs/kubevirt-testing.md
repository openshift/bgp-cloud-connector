# KubeVirt VM Testing

KubeVirt VMIs in UDN-enabled namespaces **must** use `l2bridge` binding, not `masquerade`.

The `masquerade` binding installs nftables rules in the virt-launcher pod that drop inbound traffic on UDN interfaces (`ovn-udn1`). This blocks both IPv4 and IPv6 connectivity to the VM from other pods and external hosts.

VM spec (relevant sections):

```yaml
spec:
  domain:
    devices:
      interfaces:
        - name: default
          binding:
            name: l2bridge
  networks:
    - name: default
      pod: {}
```

| Binding | Inbound to VM (UDN) | Outbound from VM |
|---------|---------------------|------------------|
| masquerade | Blocked by nftables | Works |
| bridge | Works | Works |

For a VM migration test using Migration Toolkit for Virtualization (MTV) see [test-mtv-cudn.md](test-mtv-cudn.md).

## Advertise an already-running VM with `BGPRouting.networkRef`

`TestNetworkRefVM` uses the sequence from the [Red Hat OSD on GCP guide](https://cloud.redhat.com/experts/osd/osd-bgp/)
with an independently created primary Layer 2 CUDN. The test starts a VM,
records its guest IP and boot ID, adds `BGPRouting.networkRef`, and checks the
prefix in FRR and the guest HTTP response from outside the cluster. It then
deletes the reference and checks prefix withdrawal, established peer sessions,
and continued access to the same guest from inside the CUDN.

Run it against a cluster with OpenShift Virtualization, a ready
`BGPCloudConfiguration/cluster`, and a connector build and CRD that support
`networkRef`. Select a KVM-capable router node with established IPv4 peers.
Supply an unused IPv4 subnet that does not overlap cluster or external networks.
The external client must be able to reach the CUDN via BGP once advertised.
Allow TCP port 8080 from the client to the router node. The test uses the guide's
`bridge` binding and Fedora container disk. `E2E_VM_IMAGE` can override the image
with a compatible guest that provides cloud-init and Python 3.

Set `E2E_VM_EXTERNAL_EXEC` to an executable that runs its single shell-command
argument on the external client and returns its standard output. For an IAP
client from the guide, a wrapper can use this command.

```sh
#!/bin/sh
exec gcloud compute ssh "$BGP_VM_CLIENT" --project "$GCP_PROJECT" --zone "$ZONE" \
  --tunnel-through-iap --quiet --command "$1"
```

With `oc` on the path and the desired kubeconfig context active, run the test by name.

```sh
E2E_VM_EXTERNAL_EXEC=/path/to/external-client-wrapper \
E2E_VM_CIDR=10.100.32.0/24 \
E2E_VM_ROUTER_NODE=your-router-node \
go test ./test/e2e -run '^TestNetworkRefVM$' -v -count=1 -timeout=30m
```
