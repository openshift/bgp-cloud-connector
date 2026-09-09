#!/usr/bin/env bash
#
# Remove what hack/azure/create-route-server.sh built: the BGP
# peerings, the Route Server, the public IP it required, the
# RouteServerSubnet, and the address prefix that had to be added to the
# vnet to make room for it.
#
#   hack/azure/delete-route-server.sh --dry-run
#   hack/azure/delete-route-server.sh
#
# Normally it works out what to delete from the running cluster. Once
# the cluster has gone there is nothing to ask, so name it:
#
#   INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> \
#       [AZURE_NETWORK_RESOURCE_GROUP=<group>] \
#       hack/azure/delete-route-server.sh
#
# AZURE_NETWORK_RESOURCE_GROUP defaults to AZURE_RESOURCE_GROUP and is
# only needed for a cluster installed into a vnet it does not own.
#
# Removing the address prefix as well as the subnet is the point: the
# cluster goes back to exactly what openshift-install built. The BGP
# side is a bolt-on here as it is on AWS and GCP, and a teardown that
# left the vnet a /26 wider than the installer made it would not be
# one.
#
# Order is the reverse of creation and it matters. A subnet holding a
# Route Server cannot be deleted, and an address prefix covering a
# subnet cannot be removed, so each step is only possible once the one
# before it has finished. The public IP is in use until the Route
# Server has gone. Azure enforces all of this by refusing rather than by
# corrupting, so a wrong order is loud.
#
# Safe to run when there is nothing to do: it re-derives what is left on
# every run and says so when the answer is none. That is what lets it be
# both a step of its own and something you run five times in a row to
# watch converge.
#
# Nothing stops at the first failure. Stopping is how the rest get
# orphaned, so failures are recorded and reported at the end, and the
# exit status is what says whether anything survived.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd az
require_azure

# Each can come from the environment or from a running cluster, one at a
# time rather than all or nothing, so a variable set by the caller still
# works alongside a live cluster supplying the rest.
#
# No region, and none needed: everything deleted here is named by its
# resource group. Which is just as well, because Azure's infrastructure
# status carries no region -- see azure_cluster_facts.
infra="${INFRA:-}"
rg="${AZURE_RESOURCE_GROUP:-}"
net_rg="${AZURE_NETWORK_RESOURCE_GROUP:-}"

if [[ -z "${infra}" || -z "${rg}" ]]; then
    require_cmd oc
    # Not require_cluster: it names KUBECONFIG and nothing else, and
    # here there are two ways to proceed. Somebody tearing down after
    # the cluster has gone has no kubeconfig to export and needs to be
    # told the other one.
    oc whoami >/dev/null 2>&1 \
        || die "no cluster is reachable, and INFRA was not set" \
               "This normally reads the cluster and its group from the cluster:" \
               "  export KUBECONFIG=<cluster>/auth/kubeconfig" \
               "Once the cluster has gone there is nothing to ask, so name them:" \
               "  INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> ${0##*/}" \
               "Add AZURE_NETWORK_RESOURCE_GROUP=<group> if the cluster does not own its vnet."
    require_platform Azure
    # Sets infra, rg and net_rg, and dies rather than guessing.
    azure_cluster_facts
fi
: "${net_rg:=${rg}}"

[[ -n "${infra}" && -n "${rg}" ]] \
    || die "could not work out the cluster and its resource group" \
           "No cluster is reachable, or it is not an Azure one. Name them:" \
           "  INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> ${0##*/}" \
           "Add AZURE_NETWORK_RESOURCE_GROUP=<group> if the cluster does not own its vnet." \
           "Got: infra='${infra}' rg='${rg}'"

# Checked, not kept: see the note in create-route-server.sh.
azure_subscription >/dev/null \
    || die "az has no subscription selected" \
           "Pick one: az account set --subscription <id>"

rs="${infra}-rs"
rs_pip="${infra}-rs-pip"
rs_subnet="RouteServerSubnet"

# Written by hack/azure/create-route-server.sh when it widened the vnet.
# Its absence means the prefix was already there, and is not ours.
prefix_tag="bgp-cloud-connector-added-prefix-${infra}"

# The subscription id is deliberately not printed, here or anywhere
# else in these scripts. Prow logs for openshift repositories are
# public, and it is the direct analogue of the AWS account id that
# require_aws goes out of its way not to print. The cluster, resource
# group and vnet names are printed, because they name resources that
# exist for the length of one job and they are what makes a log worth
# reading.
info "cluster:       ${infra}"
info "group:         ${rg}"
[[ "${net_rg}" != "${rg}" ]] && info "network group: ${net_rg}"

# Asked as filtered lists rather than shows, for the reason
# create-route-server.sh gives: a show cannot distinguish "it is gone"
# from "the question failed", and here reading the second as the first
# reports a teardown complete over an estate that is still billing.
#
# 0 there, 1 not there, 2 could not tell.
route_server_exists() {
    local found
    found="$(az_query "look for a route server named ${rs}" \
        az network routeserver list -g "${rg}" \
        --query "[?name=='${rs}'].name | [0]" -o tsv)" || return 2
    [[ -n "${found}" && "${found}" != "None" ]]
}

public_ip_exists() {
    local found
    found="$(az_query "look for a public IP named ${rs_pip}" \
        az network public-ip list -g "${rg}" \
        --query "[?name=='${rs_pip}'].name | [0]" -o tsv)" || return 2
    [[ -n "${found}" && "${found}" != "None" ]]
}

vnet_exists() {
    local found
    found="$(az_query "look for the vnet ${vnet}" \
        az network vnet list -g "${net_rg}" \
        --query "[?name=='${vnet}'].name | [0]" -o tsv)" || return 2
    [[ -n "${found}" && "${found}" != "None" ]]
}

subnet_prefix() {
    local prefix
    prefix="$(az_query "look for ${rs_subnet} in ${vnet}" \
        az network vnet subnet list -g "${net_rg}" --vnet-name "${vnet}" \
        --query "[?name=='${rs_subnet}'].addressPrefix | [0]" -o tsv)" || return 1
    [[ "${prefix}" == "None" ]] && prefix=""
    printf '%s' "${prefix}"
}

# The vnet is found the way the create script finds it. A failure to
# find it is recorded rather than fatal: the Route Server and the public
# IP are what bill, they are named by resource group rather than by
# vnet, and letting a vnet lookup stop the run would leave both up.
vnet=""
if ! vnet="$(azure_cluster_vnet "${net_rg}" "${infra}")"; then
    fail "cannot find the cluster's vnet in ${net_rg}; the subnet and prefix will be left"
    vnet=""
fi

# --- peerings -----------------------------------------------------------

# Deleting the Route Server takes its peerings with it, so this exists
# for the case where something is still putting them back. The operator
# recreates a peering as fast as this removes it, and the Route Server
# delete then fails against a resource that keeps returning.
#
# In CI the sequencer removes the custom resources and scales the
# operator down before this runs, so there is normally nothing here.
delete_peerings() {
    local rc=0
    route_server_exists || rc=$?
    case ${rc} in
        1) info "  no route server, so no peerings"; return 0 ;;
        2) fail "cannot tell whether the route server ${rs} exists"; return 0 ;;
    esac

    local names
    names="$(az_query "list peerings on ${rs}" \
        az network routeserver peering list -g "${rg}" --routeserver "${rs}" \
        --query '[].name' -o tsv)" || { fail "list peerings on ${rs}"; return 0; }

    if [[ -z "${names}" ]]; then
        info "  none"
        return 0
    fi

    # Only something still running can put them back, and only that is
    # worth warning about. Said rather than refused, because a teardown
    # that stops here leaves the Route Server billing.
    if oc get bgpcloudconfiguration >/dev/null 2>&1; then
        warn "  peerings exist and a BGPCloudConfiguration is still on the cluster."
        warn "  If the operator is running it will recreate them, and the route"
        warn "  server delete below will fail. Remove the CRs first:"
        warn "    hack/delete-e2e-crs.sh"
    fi

    local name
    while read -r name; do
        info "  deleting peering ${name}"
        try az network routeserver peering delete -g "${rg}" \
            --routeserver "${rs}" -n "${name}" --yes --output none \
            || fail "delete peering ${name}"
    done < <(print_fields "${names}")
}

# --- the route server ---------------------------------------------------

delete_route_server() {
    local rc=0
    route_server_exists || rc=$?
    case ${rc} in
        1) info "  already gone"; return 0 ;;
        2) fail "cannot tell whether the route server ${rs} exists"; return 0 ;;
    esac
    info "  deleting ${rs} -- slower than most of this, though quicker than creating it"
    try az network routeserver delete -g "${rg}" -n "${rs}" --yes --output none \
        || fail "delete route server ${rs}"
}

delete_public_ip() {
    local rc=0
    public_ip_exists || rc=$?
    case ${rc} in
        1) info "  already gone"; return 0 ;;
        2) fail "cannot tell whether the public IP ${rs_pip} exists"; return 0 ;;
    esac
    try az network public-ip delete -g "${rg}" -n "${rs_pip}" --output none \
        || fail "delete public IP ${rs_pip}"
}

# --- the subnet ---------------------------------------------------------

delete_subnet() {
    [[ -n "${vnet}" ]] || { info "  no vnet, so nothing to do"; return 0; }
    local rc=0
    vnet_exists || rc=$?
    case ${rc} in
        1) info "  no vnet ${vnet} in ${net_rg}"; return 0 ;;
        2) fail "cannot tell whether the vnet ${vnet} exists"; return 0 ;;
    esac

    local prefix
    prefix="$(subnet_prefix)" || { fail "cannot tell whether ${rs_subnet} exists"; return 0; }
    if [[ -z "${prefix}" ]]; then
        info "  already gone"
        return 0
    fi

    info "  deleting ${rs_subnet} (${prefix})"
    try az network vnet subnet delete -g "${net_rg}" --vnet-name "${vnet}" \
        -n "${rs_subnet}" --output none \
        || fail "delete subnet ${rs_subnet}"
}

# --- the address prefix -------------------------------------------------

# The last step, and the one that puts the vnet back as the installer
# left it. Removing a prefix means rewriting the whole list without it,
# for the same reason adding one meant rewriting the whole list with it.
#
# Only the prefix this cluster added, and only when the vnet says so.
# Guessing at the range instead -- from the subnet, or from the default
# the create script uses -- cannot tell a prefix we added from one that
# was already there and that we merely put a subnet inside. The second
# belongs to somebody else, and Azure will happily remove it as long as
# no subnet is using it, so the guess is not self-correcting.
delete_address_prefix() {
    [[ -n "${vnet}" ]] || { info "  no vnet, so nothing to do"; return 0; }
    local rc=0
    vnet_exists || rc=$?
    case ${rc} in
        1) info "  no vnet ${vnet} in ${net_rg}"; return 0 ;;
        2) fail "cannot tell whether the vnet ${vnet} exists"; return 0 ;;
    esac

    local owned
    owned="$(az_query "read the address prefix record on ${vnet}" \
        az network vnet show -g "${net_rg}" -n "${vnet}" \
        --query "tags.\"${prefix_tag}\"" -o tsv)" \
        || { fail "read the address prefix record on ${vnet}"; return 0; }
    [[ "${owned}" == "None" ]] && owned=""

    if [[ -z "${owned}" ]]; then
        info "  ${vnet} carries no record of a prefix added for ${infra}"
        info "  (nothing to remove; an address range that was here first stays)"
        return 0
    fi

    local prefixes
    prefixes="$(az_query "read the address space of ${vnet}" \
        az network vnet show -g "${net_rg}" -n "${vnet}" \
        --query 'addressSpace.addressPrefixes' -o tsv)" \
        || { fail "read the address space of ${vnet}"; return 0; }

    local -a remaining=()
    local found="" prefix
    while read -r prefix; do
        [[ -n "${prefix}" ]] || continue
        if [[ "${prefix}" == "${owned}" ]]; then
            found="yes"
        else
            remaining+=("${prefix}")
        fi
    done < <(print_fields "${prefixes}")

    if [[ -z "${found}" ]]; then
        # The record outlived the prefix, which is what a teardown
        # interrupted between the two leaves. Clear it, so a later run
        # does not keep reporting something to do.
        info "  ${owned} is already gone; clearing the record"
        try az network vnet update -g "${net_rg}" -n "${vnet}" \
            --remove "tags.${prefix_tag}" --output none \
            || fail "clear the address prefix record on ${vnet}"
        return 0
    fi

    if (( ${#remaining[@]} == 0 )); then
        warn "  ${owned} is the only prefix on ${vnet}; leaving it rather than"
        warn "  emptying the address space."
        return 0
    fi

    info "  removing ${owned}, leaving ${remaining[*]}"
    # The record goes in the same call that removes the prefix, so the
    # two cannot disagree.
    try az network vnet update -g "${net_rg}" -n "${vnet}" \
        --address-prefixes "${remaining[@]}" \
        --remove "tags.${prefix_tag}" --output none \
        || fail "remove the address prefix ${owned}"
}

info ""
info "peerings on ${rs}:"
delete_peerings
info "route server ${rs}:"
delete_route_server
info "public IP ${rs_pip}:"
delete_public_ip
info "subnet ${rs_subnet}:"
delete_subnet
info "address prefix:"
delete_address_prefix
info ""

report
