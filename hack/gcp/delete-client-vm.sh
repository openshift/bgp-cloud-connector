#!/usr/bin/env bash
#
# Remove the VM and firewall rule hack/gcp/create-client-vm.sh built.
#
#   hack/gcp/delete-client-vm.sh --dry-run
#   hack/gcp/delete-client-vm.sh
#
# The VM goes first: the rule admits its tag, and a rule without the VM
# admits nothing, where the reverse leaves a VM that can reach no pod.
#
# The zone is not known without the cluster, so the VM is found by name
# among the project's instances, compared exactly in shell for the
# reason hack/gcp/lib.sh gives.
#
# Converges rather than merely works: "already gone" is success, so a
# second run after a partial failure finishes the job.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd gcloud oc

# The identifiers come from the environment when a cluster has gone, and
# from the cluster otherwise.
infra="${INFRA:-}"
project="${GCP_PROJECT:-}"
region="${GCP_REGION:-}"
if [[ -z "${infra}" || -z "${project}" || -z "${region}" ]]; then
    require_cluster
    gcp_cluster_facts
fi

name="$(gcp_client_name "${infra}")"
firewall="$(gcp_client_firewall_name "${infra}")"

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info ""

# --- the VM ---------------------------------------------------------
info "VM ${name}:"
instances="$(gcp_query "list instances" \
    gcloud compute instances list --project="${project}" \
    --format='value(name,zone.basename())')" \
    || die "could not tell whether VM ${name} exists, so nothing was deleted"
zone="$(awk -v n="${name}" '$1 == n {print $2; exit}' <<<"${instances}")"
if [[ -n "${zone}" ]]; then
    try gcloud compute instances delete "${name}" \
        --project="${project}" --zone="${zone}" --quiet
else
    info "  already gone"
fi

# --- the firewall rule ----------------------------------------------
info "firewall ${firewall}:"
rc=0
gcp_firewall_exists "${firewall}" "${project}" || rc=$?
case "${rc}" in
    0) try gcloud compute firewall-rules delete "${firewall}" \
            --project="${project}" --quiet ;;
    1) info "  already gone" ;;
    *) die "could not tell whether firewall rule ${firewall} exists, so nothing was deleted" ;;
esac

info ""
if [[ "${dry_run}" == true ]]; then
    info "Dry run only, nothing was changed."
else
    ok "client removed"
fi
