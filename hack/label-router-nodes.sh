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
# failure rather than a cluster nobody labelled. The per-cloud
# write-e2e-profile.sh scripts write that selector from the same two
# variables, so they agree by construction.
#
# Workers only. The router nodes are where pod traffic lands, and
# peering from a master would put BGP on a node that carries none.
#
# NON_ROUTER_WORKERS=<n> leaves the last n workers, in name order,
# unlabelled, and takes the label off them if a previous run put it
# there. That is the shape the Azure data-plane spec needs: a worker
# that hosts pods but is not a BGP speaker, whose pods are reachable
# only if its own interface forwards. Unset, every worker is a router,
# as before.

set -o nounset
set -o errexit
set -o pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/common.sh
source "${repo_root}/hack/lib/common.sh"

key="${ROUTER_LABEL_KEY:-bgp_router}"
value="${ROUTER_LABEL_VALUE:-true}"
non_routers="${NON_ROUTER_WORKERS:-0}"
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

[[ "${non_routers}" =~ ^[0-9]+$ ]] \
    || die "NON_ROUTER_WORKERS is '${non_routers}', which is not a number"

# Sorted, so which workers are left out is the same on every run.
mapfile -t workers < <(print_fields "${nodes}" | sort)
routers=$(( ${#workers[@]} - non_routers ))
if [[ "${remove}" != true ]] && (( routers < 1 )); then
    die "NON_ROUTER_WORKERS=${non_routers} leaves none of the ${#workers[@]} workers as a router"
fi

for i in "${!workers[@]}"; do
    node="${workers[$i]}"
    if [[ "${remove}" == true ]] || (( i >= routers )); then
        oc label "${node}" "${key}-" --overwrite >/dev/null
        ok "unlabelled ${node#node/}"
    else
        oc label "${node}" "${key}=${value}" --overwrite >/dev/null
        ok "labelled ${node#node/} ${key}=${value}"
    fi
done

info "router nodes now: $(oc get nodes -l "${key}=${value}" -o name | wc -l)"
