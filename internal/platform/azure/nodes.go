package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// ensureNodesCanForward makes each router node's network interface accept
// packets that are not addressed to it.
//
// A packet for a pod is addressed to the pod, not to the node's interface, so
// without this Azure discards it on the host and the VM never sees it. Every
// cloud has the same requirement under a different name: AWS clears
// SourceDestCheck, GCP sets canIpForward, Azure sets enableIPForwarding. The
// polarity reads backwards on AWS alone, which is why the name of the flag
// stays down here at the client boundary and nothing above it says "IP
// forwarding".
//
// This is the failure worth being careful about: with forwarding off, BGP
// still establishes, the Route Server still learns the pod prefix, and every
// condition this operator reports is True while no packet reaches a pod.
//
// The interface is matched on the VM's ARM resource ID, which Azure records on
// it, so no naming convention has to hold.
func (p *Platform) ensureNodesCanForward(ctx context.Context, vms []VirtualMachine) error {
	logger := log.FromContext(ctx)

	// The interfaces live in the VM's resource group, which is the cluster's
	// and need not be the Route Server's, so they are listed per group rather
	// than assuming one.
	byGroup := make(map[string][]VirtualMachine)
	for _, vm := range vms {
		byGroup[vm.ResourceGroup] = append(byGroup[vm.ResourceGroup], vm)
	}

	for group, groupVMs := range byGroup {
		nics, err := p.nics.ListNICs(ctx, group)
		if err != nil {
			platform.RecordCloudAPIError(platform.PlatformAzure, platform.OpNodeForwarding)
			return fmt.Errorf("listing network interfaces in %q: %w", group, err)
		}

		for _, vm := range groupVMs {
			found := false
			for _, nic := range nics {
				if !sameResource(nic.VMID, vm.ID) {
					continue
				}
				found = true
				if nic.IPForwarding {
					continue
				}
				if err := p.nics.EnableIPForwarding(ctx, nic.ResourceGroup, nic.Name); err != nil {
					platform.RecordCloudAPIError(platform.PlatformAzure, platform.OpNodeForwarding)
					if code, refused := writeRefused(err); refused {
						return &forwardingRefusedError{nic: nic.Name, vm: vm.Name, code: code, err: err}
					}
					return fmt.Errorf("enabling IP forwarding on %q: %w", nic.Name, err)
				}
				logger.Info("enabled IP forwarding", "nic", nic.Name, "instance", vm.Name)
			}
			if !found {
				// Azure listed the interfaces without complaint; none of them
				// belongs to this VM. Not a cloud API error.
				return fmt.Errorf("no network interface is attached to %q", vm.Name)
			}
		}
	}
	return nil
}

// forwardingRefusedError is a router node whose interface has forwarding off
// and which Azure will not let this identity change.
//
// That is the expected state wherever forwarding is the platform's to set
// rather than the operator's: an identity granted only read on the
// interfaces, or a managed group behind a deny assignment. What the condition
// should say then is which node and what to do about it. Azure's own response
// is kept for errors.As but left out of the message, which would otherwise
// carry the request URL and with it the subscription id.
type forwardingRefusedError struct {
	nic, vm, code string
	err           error
}

func (e *forwardingRefusedError) Error() string {
	return fmt.Sprintf("IP forwarding is off on network interface %q of %q, and Azure refused this "+
		"identity's change to it (%s). Enable IP forwarding on the node pool, or let the identity "+
		"write the interface and join its subnet and load balancer backend pool",
		e.nic, e.vm, e.code)
}

func (e *forwardingRefusedError) Unwrap() error { return e.err }

// writeRefused reports whether Azure refused a write as not permitted, and
// with which code. A role without the action (AuthorizationFailed), one
// without a join on a resource the interface references
// (LinkedAuthorizationFailed), and a deny assignment
// (DenyAssignmentAuthorizationFailed) all arrive as 403s.
func writeRefused(err error) (string, bool) {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusForbidden {
		return "", false
	}
	return respErr.ErrorCode, true
}
