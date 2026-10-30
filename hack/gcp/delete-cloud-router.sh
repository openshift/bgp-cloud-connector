#!/usr/bin/env bash
#
# Remove the GCP estate hack/gcp/create-cloud-router.sh built.
#
#   hack/gcp/delete-cloud-router.sh --dry-run
#   hack/gcp/delete-cloud-router.sh
#
# Only what that script creates: the firewall rule, the Cloud Router,
# and the NCC hub. The peers and the router appliance spoke belong to
# the operator and go when the BGPCloudConfiguration is deleted, which
# hack/delete-e2e-crs.sh does first.
#
# Order matters and is not arbitrary. A hub refuses to go while a spoke
# is attached to it, and the spoke is the operator's, so a teardown that
# deletes the CRs afterwards would fail here and leave the hub behind.
# Deleting the router releases its interfaces, which the firewall rule
# is sourced from, so the rule goes first while its source addresses
# still mean something to read.
#
# Converges rather than merely works. Every step treats "already gone"
# as success, so a second run after a partial failure finishes the job
# instead of erroring on the half that succeeded.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd gcloud oc

# The identifiers come from the environment when a cluster has gone, and
# from the cluster otherwise. A teardown has to work after the thing it
# is tearing down stopped answering.
infra="${INFRA:-}"
project="${GCP_PROJECT:-}"
region="${GCP_REGION:-}"
if [[ -z "${infra}" || -z "${project}" || -z "${region}" ]]; then
    require_cluster
    gcp_cluster_facts
fi

hub="$(gcp_hub_name "${infra}")"
router="$(gcp_router_name "${infra}")"
firewall="$(gcp_firewall_name "${infra}")"

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info ""

# --- firewall ------------------------------------------------------
info "firewall ${firewall}:"
rc=0
gcp_firewall_exists "${firewall}" "${project}" || rc=$?
case "${rc}" in
    0) try gcloud compute firewall-rules delete "${firewall}" \
            --project="${project}" --quiet ;;
    1) info "  already gone" ;;
    *) die "could not tell whether firewall rule ${firewall} exists, so nothing was deleted" ;;
esac

# --- Cloud Router --------------------------------------------------
#
# The peers go with it. They are the operator's, and by the time this
# runs the configuration has been deleted and the operator has removed
# them; deleting the router would take any stragglers regardless.
info "Cloud Router ${router}:"
rc=0
gcp_router_exists "${router}" "${region}" "${project}" || rc=$?
case "${rc}" in
    0) try gcloud compute routers delete "${router}" \
            --project="${project}" --region="${region}" --quiet ;;
    1) info "  already gone" ;;
    *) die "could not tell whether Cloud Router ${router} exists, so nothing was deleted" ;;
esac

# --- NCC hub -------------------------------------------------------
#
# Last, and it refuses while a spoke is attached. A spoke still here
# means the operator's cleanup did not run or did not finish, and
# saying so is more useful than a bare "resource in use".
info "hub ${hub}:"
rc=0
gcp_hub_exists "${hub}" "${project}" || rc=$?
case "${rc}" in
    0)
        spokes="$(gcp_query "list spokes on ${hub}" \
            gcloud network-connectivity spokes list --project="${project}" \
            --format='value(name)' || true)"
        if [[ -n "${spokes}" ]]; then
            warn "  spokes still attached, so the hub will refuse to go:"
            while IFS= read -r s; do [[ -n "${s}" ]] && warn "    ${s}"; done <<<"${spokes}"
            warn "  those are the operator's. Remove the BGPCloudConfiguration first:"
            warn "    hack/delete-e2e-crs.sh"
        fi
        try gcloud network-connectivity hubs delete "${hub}" \
            --project="${project}" --quiet
        ;;
    1) info "  already gone" ;;
    *) die "could not tell whether hub ${hub} exists, so nothing was deleted" ;;
esac

info ""
if [[ "${dry_run}" == true ]]; then
    info "Dry run only, nothing was changed."
else
    ok "GCP estate removed"
fi
