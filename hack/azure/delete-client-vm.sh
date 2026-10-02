#!/usr/bin/env bash
#
# Remove what hack/azure/create-client-vm.sh built: the VM (its disk and
# interface go with it), its subnet, and the address prefix added to the
# vnet to make room for that subnet.
#
#   hack/azure/delete-client-vm.sh --dry-run
#   hack/azure/delete-client-vm.sh
#
# Like hack/azure/delete-route-server.sh it works out what to delete
# from the running cluster, or from the environment once the cluster has
# gone:
#
#   INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> \
#       [AZURE_NETWORK_RESOURCE_GROUP=<group>] \
#       hack/azure/delete-client-vm.sh
#
# Order is the reverse of creation: a subnet with an interface in it
# cannot be deleted, and an address prefix covering a subnet cannot be
# removed. Safe to run when there is nothing to do, and nothing stops at
# the first failure, for the reasons delete-route-server.sh gives.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd az
require_azure

delete_budget="${AZURE_DELETE_BUDGET:-1800}"

infra="${INFRA:-}"
rg="${AZURE_RESOURCE_GROUP:-}"
net_rg="${AZURE_NETWORK_RESOURCE_GROUP:-}"

if [[ -z "${infra}" || -z "${rg}" ]]; then
    require_cmd oc
    oc whoami >/dev/null 2>&1 \
        || die "no cluster is reachable, and INFRA was not set" \
               "This normally reads the cluster and its group from the cluster:" \
               "  export KUBECONFIG=<cluster>/auth/kubeconfig" \
               "Once the cluster has gone there is nothing to ask, so name them:" \
               "  INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> ${0##*/}" \
               "Add AZURE_NETWORK_RESOURCE_GROUP=<group> if the cluster does not own its vnet."
    require_platform Azure
    azure_cluster_facts
fi
: "${net_rg:=${rg}}"

[[ -n "${infra}" && -n "${rg}" ]] \
    || die "could not work out the cluster and its resource group" \
           "Got: infra='${infra}' rg='${rg}'"

azure_subscription >/dev/null \
    || die "az has no subscription selected" \
           "Pick one: az account set --subscription <id>"

vm="${infra}-e2e-client"
subnet="${infra}-e2e-client"
prefix_tag="bgp-cloud-connector-added-client-prefix-${infra}"
subnet_tag="bgp-cloud-connector-added-client-subnet-${infra}"

info "cluster:       ${infra}"
info "group:         ${rg}"
[[ "${net_rg}" != "${rg}" ]] && info "network group: ${net_rg}"

# 0 there, 1 not there, 2 could not tell.
vm_exists() {
    local found
    found="$(az_query "look for a VM named ${vm}" \
        az vm list -g "${net_rg}" \
        --query "[?name=='${vm}'].name | [0]" -o tsv)" || return 2
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
    prefix="$(az_query "look for ${subnet} in ${vnet}" \
        az network vnet subnet list -g "${net_rg}" --vnet-name "${vnet}" \
        --query "[?name=='${subnet}'].addressPrefix | [0]" -o tsv)" || return 1
    [[ "${prefix}" == "None" ]] && prefix=""
    printf '%s' "${prefix}"
}

# Reads one of our records off the vnet. Prints the value, or nothing
# when there is no record.
vnet_record() {
    local tag="$1" value
    value="$(az_query "read the ${tag} record on ${vnet}" \
        az network vnet show -g "${net_rg}" -n "${vnet}" \
        --query "tags.\"${tag}\"" -o tsv)" || return 1
    [[ "${value}" == "None" ]] && value=""
    printf '%s' "${value}"
}

# A failure to find the vnet is recorded rather than fatal: the VM is
# what bills, it is named by resource group rather than by vnet, and a
# vnet lookup must not stop it being deleted.
vnet=""
if ! vnet="$(azure_cluster_vnet "${net_rg}" "${infra}")"; then
    fail "cannot find the cluster's vnet in ${net_rg}; the subnet and prefix will be left"
    vnet=""
fi

# --- the vm -------------------------------------------------------------

delete_vm() {
    local rc=0
    vm_exists || rc=$?
    case ${rc} in
        1) info "  already gone"; return 0 ;;
        2) fail "cannot tell whether the VM ${vm} exists"; return 0 ;;
    esac
    # Its disk and interface were created with delete options, so they
    # go with it.
    info "  deleting ${vm} (with its disk and interface)"
    az_retry "delete VM ${vm}" "${delete_budget}" \
        az vm delete -g "${net_rg}" -n "${vm}" --yes --output none \
        || fail "delete VM ${vm}"
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

    local owned
    owned="$(vnet_record "${subnet_tag}")" \
        || { fail "read the subnet record on ${vnet}"; return 0; }
    if [[ -z "${owned}" ]]; then
        info "  ${vnet} carries no record of a client subnet created for ${infra}"
        return 0
    fi

    local prefix
    prefix="$(subnet_prefix)" || { fail "cannot tell whether ${subnet} exists"; return 0; }
    if [[ -n "${prefix}" ]]; then
        info "  deleting ${subnet} (${prefix})"
        az_retry "delete subnet ${subnet}" "${delete_budget}" \
            az network vnet subnet delete -g "${net_rg}" --vnet-name "${vnet}" \
            -n "${subnet}" --output none \
            || { fail "delete subnet ${subnet}"; return 0; }
    else
        info "  already gone; clearing the record"
    fi

    # Only once the subnet is actually gone, so a failed delete leaves
    # the record for the next run to act on.
    try az network vnet update -g "${net_rg}" -n "${vnet}" \
        --remove "tags.${subnet_tag}" --output none \
        || fail "clear the subnet record on ${vnet}"
}

# --- the address prefix -------------------------------------------------

# Only the prefix this cluster added, and only when the vnet says so, for
# the reason delete-route-server.sh gives.
delete_address_prefix() {
    [[ -n "${vnet}" ]] || { info "  no vnet, so nothing to do"; return 0; }
    local rc=0
    vnet_exists || rc=$?
    case ${rc} in
        1) info "  no vnet ${vnet} in ${net_rg}"; return 0 ;;
        2) fail "cannot tell whether the vnet ${vnet} exists"; return 0 ;;
    esac

    local owned
    owned="$(vnet_record "${prefix_tag}")" \
        || { fail "read the address prefix record on ${vnet}"; return 0; }
    if [[ -z "${owned}" ]]; then
        info "  ${vnet} carries no record of a client prefix added for ${infra}"
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
    az_retry "remove the address prefix ${owned}" "${delete_budget}" \
        az network vnet update -g "${net_rg}" -n "${vnet}" \
        --address-prefixes "${remaining[@]}" \
        --remove "tags.${prefix_tag}" --output none \
        || fail "remove the address prefix ${owned}"
}

info ""
info "vm ${vm}:"
delete_vm
info "subnet ${subnet}:"
delete_subnet
info "address prefix:"
delete_address_prefix
info ""

report
