#!/usr/bin/env bash
#
# Create the GCP estate a BGPCloudConfiguration expects to discover.
#
#   hack/gcp/create-cloud-router.sh --dry-run
#   hack/gcp/create-cloud-router.sh
#
# Needs credentials for the project owning the cluster and a KUBECONFIG:
# the infra id, project and region come from the running cluster rather
# than from arguments.
#
# Prerequisites only, which is the whole design of this file. The
# operator creates the BGP peers, the router appliance spoke, and flips
# canIpForward and nested virtualisation on the router nodes, and it
# orders them itself -- forwarding before the spoke, because GCP rejects
# a spoke whose instances cannot forward, and the spoke before the
# peers, because a VM cannot peer with a Cloud Router until it belongs
# to one. So this builds only what the operator never builds: the NCC
# hub, the Cloud Router with its two interfaces, and the firewall rule.
#
# Creating the peers here would be worse than redundant. GCP refuses two
# peers sharing a peer IP on one interface, so the operator's own peers
# are rejected and the configuration sits Degraded on
# "Invalid value for field 'resource.bgpPeers[].peerIpAddress'" --
# measured on 2026-09-11, against an estate a script had helpfully
# finished for it. It would also let a completely broken operator adopt
# a working estate and look identical to a healthy one, which is the
# same reason hack/azure/create-route-server.sh stops where it does.
#
# Two interfaces, not one. A Cloud Router presents its interfaces as the
# BGP neighbours, and the redundant pair is what gives the cluster two
# to talk to; one would work and halve the redundancy for nothing.
#
# ASN sets the GCP side and must differ from the localASN in your
# BGPCloudConfiguration, or the session is not eBGP.
#
# Rerunning adopts what already exists rather than creating a second
# estate, so it is safe as a check on the current state.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

asn="${ASN:-65000}"

parse_args "$@"
require_cmd gcloud oc
require_cluster
require_platform GCP

gcp_cluster_facts

hub="$(gcp_hub_name "${infra}")"
router="$(gcp_router_name "${infra}")"
firewall="$(gcp_firewall_name "${infra}")"

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info "asn:      ${asn} (the cluster's localASN must differ)"

# The network and the worker subnet the installer built. Both are read
# rather than assumed: a cluster can be given a network, and the name
# then follows no convention of ours.
network="$(gcp_query "read the cluster's network" \
    gcloud compute networks list --project="${project}" \
    --filter="name~^${infra}" --format='value(name)' | head -1)"
[[ -n "${network}" ]] || die "no network found for ${infra} in ${project}"

subnet="$(gcp_query "read the cluster's worker subnet" \
    gcloud compute networks subnets list --project="${project}" \
    --regions="${region}" --filter="name~^${infra}.*worker" \
    --format='value(name)' | head -1)"
[[ -n "${subnet}" ]] || die "no worker subnet found for ${infra} in ${region}"

info "network:  ${network}"
info "subnet:   ${subnet}"
info ""

# --- NCC hub -------------------------------------------------------
info "hub ${hub}:"
rc=0
gcp_hub_exists "${hub}" "${project}" || rc=$?
case "${rc}" in
    0) info "  adopting the hub that is already there" ;;
    1) try gcloud network-connectivity hubs create "${hub}" \
            --project="${project}" \
            --description="CUDN BGP routing for ${infra}" ;;
    *) die "could not tell whether hub ${hub} exists, so nothing was created" ;;
esac

# --- Cloud Router --------------------------------------------------
info "Cloud Router ${router}:"
rc=0
gcp_router_exists "${router}" "${region}" "${project}" || rc=$?
case "${rc}" in
    0) info "  adopting the Cloud Router that is already there" ;;
    1)
        try gcloud compute routers create "${router}" \
            --project="${project}" --region="${region}" \
            --network="${network}" --asn="${asn}" \
            --description="CUDN BGP routing for ${infra}"
        # The second interface names the first as its redundant pair,
        # so the order matters and they cannot be created together.
        try gcloud compute routers add-interface "${router}" \
            --project="${project}" --region="${region}" \
            --interface-name="${infra}-cr-if-0" --subnetwork="${subnet}"
        try gcloud compute routers add-interface "${router}" \
            --project="${project}" --region="${region}" \
            --interface-name="${infra}-cr-if-1" --subnetwork="${subnet}" \
            --redundant-interface="${infra}-cr-if-0"
        ;;
    *) die "could not tell whether Cloud Router ${router} exists, so nothing was created" ;;
esac

# --- firewall ------------------------------------------------------
#
# GCP denies ingress by default and the installer opens 6443, etcd, node
# ports and geneve, never 179. Without this the sessions sit in Connect
# and everything else looks healthy.
info "firewall ${firewall}:"
rc=0
gcp_firewall_exists "${firewall}" "${project}" || rc=$?
if (( rc == 2 )); then
    die "could not tell whether firewall rule ${firewall} exists, so nothing was created"
elif (( rc == 0 )); then
    info "  adopting the rule that is already there"
else
    # Sourced from the router's own interface addresses rather than from
    # the subnet, so the rule is as narrow as the thing it exists for.
    # They do not exist until the router does, which is why this is last.
    sources="$(gcp_router_interface_addresses "${router}" "${region}" "${project}" \
        | paste -sd, -)"
    [[ -n "${sources}" ]] || die "the Cloud Router reported no interface addresses, so the firewall rule would allow nothing"
    try gcloud compute firewall-rules create "${firewall}" \
        --project="${project}" --network="${network}" \
        --direction=INGRESS --action=allow --rules=tcp:179 \
        --source-ranges="${sources}" --priority=900 \
        --target-tags="${infra}-worker"
fi

info ""
if [[ "${dry_run}" == true ]]; then
    info "Dry run only, nothing was changed."
else
    ok "GCP estate ready"
    info "the operator creates the peers, the spoke and the forwarding flags"
fi
