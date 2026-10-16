#!/usr/bin/env bash
#
# Grant the operator's identity a role on the Route Server alone, rather
# than on the resource group that holds it.
#
#   ROUTE_SERVER_ASSIGNEE=<object-id> hack/azure/grant-route-server-role.sh --dry-run
#   ROUTE_SERVER_ASSIGNEE=<object-id> hack/azure/grant-route-server-role.sh
#
# ROUTE_SERVER_ASSIGNEE is the object (principal) id of the identity the
# operator runs as. ROUTE_SERVER_ROLE defaults to Network Contributor,
# the narrowest built-in role that can delete a Route Server's BGP
# connections; a CI principal that may not create custom roles can still
# assign it.
#
# A separate step from hack/azure/create-route-server.sh because the
# grant is not estate: the Route Server exists for the cluster, the role
# assignment for one identity. It runs after the Route Server exists,
# which is why CI cannot make it before the test starts.
#
# Covers the Route Server's peerings only. Writing a router node's
# interface also checks virtualNetworks/subnets/join/action on the
# subnet it sits in, which is granted where that subnet is known.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd az oc
require_cluster
require_platform Azure
require_azure

assignee="${ROUTE_SERVER_ASSIGNEE:-}"
role="${ROUTE_SERVER_ROLE:-Network Contributor}"
[[ -n "${assignee}" ]] \
    || die "ROUTE_SERVER_ASSIGNEE is not set" \
           "Name the object id of the identity the operator runs as:" \
           "  ROUTE_SERVER_ASSIGNEE=<object-id> ${0##*/}"

azure_cluster_facts
rs="${infra}-rs"

info "cluster:       ${infra}"
info "route server:  ${rs}"
info "role:          ${role}"

scope="$(az_query "read the id of ${rs}" \
    az network routeserver show -g "${net_rg}" -n "${rs}" --query id -o tsv)" \
    || die "cannot read the id of ${rs}" \
           "Create it first: hack/azure/create-route-server.sh"
[[ -n "${scope}" && "${scope}" != "None" ]] \
    || die "${rs} reports no id"

azure_ensure_role_assignment "${assignee}" "${role}" "${scope}" \
    || die "cannot grant ${role} on ${rs}"

ok "granted ${role} on ${rs}"
report
