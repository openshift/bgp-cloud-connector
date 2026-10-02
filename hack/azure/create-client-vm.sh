#!/usr/bin/env bash
#
# Stand up a VM in the cluster's virtual network, outside the cluster,
# for the e2e suite to send traffic from.
#
#   hack/azure/create-client-vm.sh --dry-run
#   hack/azure/create-client-vm.sh
#
# Everything else the suite checks is the operator's own state: the
# peerings it made, the interfaces it wrote, the conditions it reports.
# None of that shows a packet arriving. With forwarding off on a node's
# interface BGP still establishes and every condition stays True while
# no reply leaves the node, so the only proof that a pod on the
# advertised network is reachable is something outside the cluster
# reaching it. This is that something.
#
# It sits in its own subnet rather than in the worker subnet, so that
# the route it uses is the one the Route Server learned and programmed
# into the vnet, and not a path that exists only because it shares a
# link with the nodes.
#
# The suite drives it with `az vm run-command`, which goes through the
# Azure VM agent rather than the network. So it has no public IP, no
# network security group and no SSH key, and nothing can reach it from
# outside the vnet.
#
# Rerunning adopts whatever already exists, so it doubles as a check on
# the current state. hack/azure/delete-client-vm.sh removes it again.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# Its own range, for the reason ROUTE_SERVER_CIDR has one: a vnet
# openshift-install built is split entirely between the master and
# worker subnets, so there is no room for a third without widening it.
#
# Beside the Route Server's 10.1.0.0/26 rather than inside it: Azure
# requires RouteServerSubnet to have its prefix to itself. Must not
# overlap machineNetwork, clusterNetwork, serviceNetwork or the
# advertised network's own subnet.
client_cidr="${CLIENT_CIDR:-10.1.0.64/28}"

# The smallest general-purpose size offered in every region these jobs
# use. It runs curl and nothing else.
client_size="${CLIENT_VM_SIZE:-Standard_B2ls_v2}"

# Any image with curl in it. Ubuntu ships it, which saves installing
# anything on a VM that has no route to a package mirror.
client_image="${CLIENT_VM_IMAGE:-Ubuntu2404}"

parse_args "$@"
require_cmd az oc
require_cluster
require_platform Azure
require_azure

azure_cluster_facts
# Checked, not kept: see the note in create-route-server.sh.
azure_subscription >/dev/null \
    || die "az has no subscription selected" \
           "Pick one: az account set --subscription <id>"
region="$(azure_group_location "${net_rg}")" \
    || die "resource group ${net_rg} is not visible in the subscription az is pointed at" \
           "The cluster is probably in a different subscription from that one." \
           "Switch: az account set --subscription <id>"
vnet="$(azure_cluster_vnet "${net_rg}" "${infra}")" \
    || die "cannot find the cluster's virtual network in ${net_rg}"

vm="${infra}-e2e-client"
subnet="${infra}-e2e-client"

# What says the widening and the subnet were ours, keyed on the cluster
# for the reason create-route-server.sh gives. Separate from the Route
# Server's records, so either estate can be removed without the other.
prefix_tag="bgp-cloud-connector-added-client-prefix-${infra}"
subnet_tag="bgp-cloud-connector-added-client-subnet-${infra}"

info "cluster:       ${infra}"
info "region:        ${region}"
info "group:         ${rg}"
[[ "${net_rg}" != "${rg}" ]] && info "network group: ${net_rg} (a vnet the cluster does not own)"
info "vnet:          ${vnet}"
info "client vm:     ${vm}"

# Asked as filtered lists rather than shows, for the reason
# create-route-server.sh gives. 0 there, 1 not there, 2 could not tell.
vm_exists() {
    local found
    found="$(az_query "look for a VM named ${vm}" \
        az vm list -g "${net_rg}" \
        --query "[?name=='${vm}'].name | [0]" -o tsv)" || return 2
    [[ -n "${found}" && "${found}" != "None" ]]
}

# Prints the prefix, or nothing when there is no such subnet. Non-zero
# only when the question could not be asked.
subnet_prefix() {
    local prefix
    prefix="$(az_query "look for ${subnet} in ${vnet}" \
        az network vnet subnet list -g "${net_rg}" --vnet-name "${vnet}" \
        --query "[?name=='${subnet}'].addressPrefix | [0]" -o tsv)" || return 1
    [[ "${prefix}" == "None" ]] && prefix=""
    printf '%s' "${prefix}"
}

# --- the address range --------------------------------------------------

# The same widening create-route-server.sh does, for a second range.
# update replaces the address space wholesale, so every existing prefix
# is repeated or it is dropped, and the record goes in the same call.
ensure_address_prefix() {
    local prefixes have prefix
    prefixes="$(az_query "read the address space of ${vnet}" \
        az network vnet show -g "${net_rg}" -n "${vnet}" \
        --query 'addressSpace.addressPrefixes' -o tsv)" \
        || die "cannot read the address space of ${vnet}"

    have=""
    local -a all=()
    while read -r prefix; do
        [[ "${prefix}" == "${client_cidr}" ]] && have="yes"
        [[ -n "${prefix}" ]] && all+=("${prefix}")
    done < <(print_fields "${prefixes}")

    if [[ -n "${have}" ]]; then
        info "  ${client_cidr} is already in the address space"
        return 0
    fi

    info "  adding ${client_cidr} to ${vnet} (existing subnets untouched)"
    all+=("${client_cidr}")
    try az network vnet update -g "${net_rg}" -n "${vnet}" \
        --address-prefixes "${all[@]}" \
        --set "tags.${prefix_tag}=${client_cidr}" --output none
}

# --- the subnet ---------------------------------------------------------

ensure_subnet() {
    local prefix
    prefix="$(subnet_prefix)" || die "cannot tell whether ${subnet} exists in ${vnet}"

    if [[ -n "${prefix}" ]]; then
        info "  adopting the existing ${subnet} (${prefix})"
        return 0
    fi

    info "  creating ${subnet} (${client_cidr})"
    try az network vnet subnet create -g "${net_rg}" --vnet-name "${vnet}" \
        -n "${subnet}" --address-prefixes "${client_cidr}" --output none

    # A separate call, as in create-route-server.sh. A run interrupted
    # between the two leaves an unrecorded subnet, which the teardown
    # then leaves alone.
    try az network vnet update -g "${net_rg}" -n "${vnet}" \
        --set "tags.${subnet_tag}=${subnet}" --output none
}

# --- the vm -------------------------------------------------------------

# A password rather than an SSH key, because nothing ever logs in:
# run-command goes through the VM agent. A key would mean writing one
# into ~/.ssh, which in a CI pod may not be writable and at a desk
# overwrites nothing only by luck. The password is random, satisfies
# Azure's complexity rule by construction, and is never printed or kept.
#
# head reads a fixed amount and finishes before anything downstream
# does. Reading /dev/urandom with tr and stopping it with head instead
# kills tr with SIGPIPE, which pipefail turns into a failure, and the
# password comes back empty.
random_password() {
    local tail
    tail="$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')"
    [[ -n "${tail}" ]] || die "cannot generate a password for ${vm}"
    printf 'Aa1!%s' "${tail}"
}

ensure_vm() {
    local rc=0
    vm_exists || rc=$?
    case ${rc} in
        0) info "  adopting the existing ${vm}"; return 0 ;;
        2) die "cannot tell whether the VM ${vm} exists" ;;
    esac

    info "  creating ${vm} (${client_size}, ${client_image}, no public IP)"
    # The disk and the interface go with the VM, so deleting the VM is
    # the whole teardown and nothing it created can be orphaned by a
    # teardown that stops part way.
    try az vm create -g "${net_rg}" -n "${vm}" \
        --location "${region}" \
        --image "${client_image}" --size "${client_size}" \
        --vnet-name "${vnet}" --subnet "${subnet}" \
        --public-ip-address "" --nsg "" \
        --authentication-type password \
        --admin-username e2e --admin-password "$(random_password)" \
        --os-disk-delete-option Delete --nic-delete-option Delete \
        --output none
}

info ""
info "address range ${client_cidr}:"
ensure_address_prefix
info "subnet ${subnet}:"
ensure_subnet
info "vm ${vm}:"
ensure_vm
info ""

if [[ "${dry_run}" == true ]]; then
    info "would then read back the VM's address"
    report
    exit $?
fi

address="$(az_query "read the address of ${vm}" \
    az vm list-ip-addresses -g "${net_rg}" -n "${vm}" \
    --query '[0].virtualMachine.network.privateIpAddresses[0]' -o tsv)" \
    || die "cannot read back the address of ${vm}"
info "client vm address: ${address}"

ok "client vm ready"
report
