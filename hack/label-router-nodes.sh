#!/usr/bin/env bash
#
# Label the nodes the operator should peer from.
#
#   hack/label-router-nodes.sh
#   hack/label-router-nodes.sh --remove
#
# The label has to match spec.routerNodeSelector in the BGPCloudConfiguration
# being used, or the operator selects nothing, builds no peers, and
# reports a plan with no groups in it -- which looks like a discovery
# failure rather than a cluster nobody labelled. hack/aws/write-e2e-profile.sh
# writes that selector from the same two variables, so they agree by
# construction.
#
# Workers only. The router nodes are where pod traffic lands, and
# peering from a master would put BGP on a node that carries none.

set -o nounset
set -o errexit
set -o pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/common.sh
source "${repo_root}/hack/lib/common.sh"

key="${ROUTER_LABEL_KEY:-bgp_router}"
value="${ROUTER_LABEL_VALUE:-true}"
remove=false

for arg in "$@"; do
    case "${arg}" in
        --remove) remove=true ;;
        *) die "unknown option: ${arg}" "Usage: ${0##*/} [--remove]" ;;
    esac
done

require_cmd oc
require_cluster

nodes="$(oc get nodes -l node-role.kubernetes.io/worker -o name)" \
    || die "cannot list worker nodes"
[[ -n "${nodes}" ]] || die "no nodes with the worker role" \
    "The operator peers from workers; a cluster with none has nothing to label."

for node in ${nodes}; do
    if [[ "${remove}" == true ]]; then
        oc label "${node}" "${key}-" --overwrite >/dev/null
        ok "unlabelled ${node#node/}"
    else
        oc label "${node}" "${key}=${value}" --overwrite >/dev/null
        ok "labelled ${node#node/} ${key}=${value}"
    fi
done

info "router nodes now: $(oc get nodes -l "${key}=${value}" -o name | wc -l)"
