#!/usr/bin/env bash
#
# Stand up the Azure Route Server estate a BGPCloudConfiguration needs,
# on a cluster openshift-install built knowing nothing about BGP.
#
#   hack/azure/create-route-server.sh --dry-run
#   hack/azure/create-route-server.sh
#
# Singular, unlike the AWS script, because Azure allows exactly one
# Route Server per virtual network. There is no per-zone endpoint to
# spread across either: a Route Server presents a redundant pair of
# addresses for the whole vnet and every node peers with both whatever
# zone it is in. That is why the operator's Azure discovery emits one
# peer group where AWS emits one per availability zone.
#
# This creates what the operator never creates and nothing else. The
# peerings are the operator's own work and the suite asserts on them,
# so building them here would let a completely broken operator adopt
# them and look identical to a working one.
#
# Rerunning adopts whatever already exists rather than building a
# second estate, so it doubles as a check on the current state.
#
# The Route Server and its public IP both bill hourly. They live in the
# cluster's own resource group, so a cluster teardown takes them with
# it -- unlike the AWS endpoints, which belong to the VPC and outlive
# the cluster. Tear them down explicitly if you are keeping the
# cluster: hack/azure/delete-route-server.sh.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# The range RouteServerSubnet is carved from, and the one thing here
# that reaches into infrastructure the installer owns.
#
# Azure requires the Route Server to sit in a dedicated subnet named
# exactly RouteServerSubnet, minimum /26, which cannot share the master
# or worker subnet and cannot carry an NSG or a route table -- Azure
# rejects both. openshift-install sets the vnet address space equal to
# machineNetwork and splits all of it between those two, so a cluster it
# built has no room for a third subnet at any machineNetwork size:
# shrinking machineNetwork just yields a smaller vnet that is equally
# full. Extending the vnet by one /26 is therefore the least that lets
# the bolt-on model work on Azure at all.
#
# Must not overlap machineNetwork, clusterNetwork or serviceNetwork.
# 10.1.0.0/26 clears the installer's 10.0.0.0/16 default.
rs_cidr="${ROUTE_SERVER_CIDR:-10.1.0.0/26}"

# How long to wait for a Route Server to report its addresses.
#
# A create measured 15m10s on 2026-09-09, in a subscription nobody else
# was using. CI is not that: the quota slices are shared and Azure's own
# FAQ puts Route Server deployment at 30 to 60 minutes once a virtual
# network gateway is involved, which is the same control plane under
# load. So this is an hour rather than twice the measurement, because
# the two failure modes are not comparable -- waiting longer costs
# nothing when the estate is coming up anyway, and giving up early
# fails a job that would have passed and leaves a half-built Route
# Server behind for the teardown to find.
#
# It is only ever spent adopting one an interrupted run left unfinished.
# A create that ran to completion is already ready when this is reached.
ready_timeout="${ROUTE_SERVER_READY_TIMEOUT:-3600}"

parse_args "$@"
require_cmd az oc
require_cluster
require_platform Azure
require_azure

azure_cluster_facts
# Checked, not kept. Nothing here needs the id, and printing it is
# what the note below refuses to do; what matters is that az has a
# subscription selected at all, because every lookup after this is
# scoped to it and an unset one fails them all with the same
# unhelpful error.
azure_subscription >/dev/null \
    || die "az has no subscription selected" \
           "Pick one: az account set --subscription <id>"
region="$(azure_group_location "${rg}")" \
    || die "resource group ${rg} is not visible in the subscription az is pointed at" \
           "The cluster is probably in a different subscription from that one." \
           "Switch: az account set --subscription <id>"
vnet="$(azure_cluster_vnet "${net_rg}" "${infra}")" \
    || die "cannot find the cluster's virtual network in ${net_rg}"

# Named for the cluster, as on AWS and GCP, so everything created here
# is identifiable as one cluster's and nothing collides in a shared
# subscription.
#
# The subnet is the exception and is not ours to name: Azure requires it
# to be called exactly RouteServerSubnet and rejects --hosted-subnet
# pointing at anything else.
rs="${infra}-rs"
rs_pip="${infra}-rs-pip"
rs_subnet="RouteServerSubnet"

# What says the widening was ours.
#
# The teardown removes an address prefix only when this tag records that
# this cluster added it. Without that record it cannot tell a prefix we
# added from one that was already there when we arrived and that we
# merely put a subnet inside, and removing the second narrows a vnet
# somebody widened for their own reasons.
#
# Keyed on the cluster, because a vnet the cluster does not own can hold
# more than one cluster's estate, and an unkeyed tag would let the
# second overwrite the first's record.
prefix_tag="bgp-cloud-connector-added-prefix-${infra}"

# The subscription id is deliberately not printed, here or anywhere
# else in these scripts. Prow logs for openshift repositories are
# public, and it is the direct analogue of the AWS account id that
# require_aws goes out of its way not to print. The cluster, resource
# group and vnet names are printed, because they name resources that
# exist for the length of one job and they are what makes a log worth
# reading.
info "cluster:       ${infra}"
info "region:        ${region}"
info "group:         ${rg}"
[[ "${net_rg}" != "${rg}" ]] && info "network group: ${net_rg} (a vnet the cluster does not own)"
info "vnet:          ${vnet}"
info "route server:  ${rs} (Azure fixes its ASN at 65515)"

# Existence is asked as a filtered list rather than as a show.
#
# `az ... show` on something absent fails, and so does `az ... show`
# when the login has expired or the subscription is wrong, with no way
# to tell the two apart from the status. Read as "it is not there" the
# second case builds a second estate beside the first. A list filtered
# by name answers absence with an empty result and zero, and answers
# failure with a non-zero status, which is the distinction every
# adoption below depends on.
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

# Prints the prefix, or nothing when there is no such subnet. Non-zero
# only when the question could not be asked.
subnet_prefix() {
    local prefix
    prefix="$(az_query "look for ${rs_subnet} in ${vnet}" \
        az network vnet subnet list -g "${net_rg}" --vnet-name "${vnet}" \
        --query "[?name=='${rs_subnet}'].addressPrefix | [0]" -o tsv)" || return 1
    [[ "${prefix}" == "None" ]] && prefix=""
    printf '%s' "${prefix}"
}

# Readiness is the addresses, not provisioningState.
#
# Measured on 2026-09-09, eleven minutes into a fifteen-minute create:
# the virtual hub already reported provisioningState Succeeded with
# virtualRouterIps empty. az waits for the real thing when it is the one
# doing the creating, but a create that was interrupted -- your Ctrl-C,
# or prow's TERM -- leaves a Route Server that exists, claims Succeeded,
# and has no addresses. Adopting that hands back something nothing can
# peer with, and the failure arrives later looking like BGP.
route_server_addresses() {
    az_query "read the addresses of ${rs}" \
        az network routeserver show -g "${rg}" -n "${rs}" \
        --query 'virtualRouterIps[]' -o tsv
}

# wait_until's contract: 0 ready, 1 not yet, 2 no point waiting.
#
# A failed read is not on its own a reason to stop waiting. The budget
# above is an hour precisely because the control plane may be busy, and
# handing back 2 on the first throttled describe would abandon the wait
# for the same reason it was made long. So a few consecutive failures
# count as "not yet" and only a run of them gives up, which keeps a
# genuine failure -- an expired login, a deleted Route Server -- from
# burning the whole hour.
ready_read_failures=0
ready_read_failures_max="${ROUTE_SERVER_READ_FAILURES:-3}"

route_server_ready() {
    local ips
    if ! ips="$(route_server_addresses)"; then
        ready_read_failures=$(( ready_read_failures + 1 ))
        if (( ready_read_failures >= ready_read_failures_max )); then
            return 2
        fi
        warn "  read ${ready_read_failures} of ${ready_read_failures_max} failed; still waiting"
        return 1
    fi
    ready_read_failures=0
    [[ -n "${ips}" ]]
}

# --- the address range --------------------------------------------------

# Adding a prefix disturbs neither the existing subnets nor anything on
# them, but it is still a change to something openshift-install made, so
# it is said out loud rather than done quietly.
# hack/azure/delete-route-server.sh removes it again.
ensure_address_prefix() {
    local prefixes have
    prefixes="$(az_query "read the address space of ${vnet}" \
        az network vnet show -g "${net_rg}" -n "${vnet}" \
        --query 'addressSpace.addressPrefixes' -o tsv)" \
        || die "cannot read the address space of ${vnet}"

    have=""
    while read -r prefix; do
        [[ "${prefix}" == "${rs_cidr}" ]] && have="yes"
    done < <(print_fields "${prefixes}")

    if [[ -n "${have}" ]]; then
        # Adopted, not added. Deliberately not tagged: the teardown
        # leaves an untagged prefix alone, which is what stops it
        # removing address space that was here before we were.
        info "  ${rs_cidr} is already in the address space, so it is not ours"
        info "  (the teardown will leave it where it found it)"
        return 0
    fi

    info "  adding ${rs_cidr} to ${vnet} (installer-owned; existing subnets untouched)"
    # update replaces the list wholesale, so the existing prefixes have
    # to be repeated or they are dropped, which would orphan every
    # subnet in the vnet.
    local -a all=()
    while read -r prefix; do
        [[ -n "${prefix}" ]] && all+=("${prefix}")
    done < <(print_fields "${prefixes}")
    all+=("${rs_cidr}")

    # The record is written by the same call that widens the vnet, so
    # there is no window in which the vnet is wider than the installer
    # made it with nothing to say who widened it. --set leaves the
    # installer's own tags alone, checked against a live vnet.
    try az network vnet update -g "${net_rg}" -n "${vnet}" \
        --address-prefixes "${all[@]}" \
        --set "tags.${prefix_tag}=${rs_cidr}" --output none
}

# --- the subnet ---------------------------------------------------------

ensure_subnet() {
    local prefix
    prefix="$(subnet_prefix)" || die "cannot tell whether ${rs_subnet} exists in ${vnet}"

    if [[ -n "${prefix}" ]]; then
        info "  adopting the existing ${rs_subnet} (${prefix})"
        return 0
    fi

    info "  creating ${rs_subnet} (${rs_cidr})"
    # No NSG and no route table, and not by omission: Azure supports
    # neither on this subnet and rejects both, so there is nothing to
    # decide.
    try az network vnet subnet create -g "${net_rg}" --vnet-name "${vnet}" \
        -n "${rs_subnet}" --address-prefixes "${rs_cidr}" --output none
}

# --- the public IP ------------------------------------------------------

# Required by the API and not for anything you route through: Azure uses
# it to reach the Route Server's own management backend, and no cluster
# traffic goes near it. Standard SKU with static allocation is the only
# combination it accepts. It bills like any other static address.
ensure_public_ip() {
    local rc=0
    public_ip_exists || rc=$?
    case ${rc} in
        0) info "  adopting the existing ${rs_pip}"; return 0 ;;
        2) die "cannot tell whether the public IP ${rs_pip} exists" ;;
    esac

    info "  creating ${rs_pip} (Standard, static)"
    try az network public-ip create -g "${rg}" -n "${rs_pip}" \
        --sku Standard --allocation-method Static --version IPv4 \
        --location "${region}" --output none
}

# --- the route server ---------------------------------------------------

# Azure's own FAQ puts deployment at 30 to 60 minutes; measured here at
# about thirteen. Said out loud because this is where a run sits, and a
# wait that long with nothing printed reads as a hang rather than as the
# documented behaviour. az blocks until the operation completes, so
# there is nothing to poll and no partial state to reconcile.
#
# branch-to-branch and hub-routing-preference are left at their
# defaults. They govern how a Route Server arbitrates between
# ExpressRoute and VPN gateways, and there are none here, so setting
# them would be copying a reference estate rather than configuring
# anything.
ensure_route_server() {
    local rc=0
    route_server_exists || rc=$?
    case ${rc} in
        0) info "  adopting the existing Route Server ${rs}"; return 0 ;;
        2) die "cannot tell whether the Route Server ${rs} exists" ;;
    esac

    # Not looked up in a rehearsal, because the subnet the id belongs to
    # is one of the things a rehearsal has not created. Asking anyway
    # prints a NotFound in the path where NotFound is the expected
    # answer, which teaches you to read past exactly the errors this
    # library exists to make you read.
    local subnet_id
    if [[ "${dry_run}" == true ]]; then
        subnet_id="<id of ${rs_subnet}, which a rehearsal has not created>"
    else
        # ensure_subnet ran before this and either adopted or created
        # it, so a failure here is a failure and not an absence. Saying
        # "no subnet" instead would name a cause we did not observe, and
        # send whoever reads it to build a subnet that already exists.
        subnet_id="$(az_query "read the id of ${rs_subnet}" \
            az network vnet subnet show -g "${net_rg}" --vnet-name "${vnet}" \
            -n "${rs_subnet}" --query id -o tsv)" \
            || die "cannot read the id of ${rs_subnet}, which ${0##*/} just ensured exists"
        [[ -n "${subnet_id}" && "${subnet_id}" != "None" ]] \
            || die "${rs_subnet} exists in ${vnet} but Azure reports no id for it"
    fi

    info "  creating ${rs} -- expect this to take about fifteen minutes"
    try az network routeserver create -g "${rg}" -n "${rs}" \
        --hosted-subnet "${subnet_id}" \
        --public-ip-address "${rs_pip}" \
        --location "${region}" --output none
}

info ""
info "address range ${rs_cidr}:"
ensure_address_prefix
info "subnet ${rs_subnet}:"
ensure_subnet
info "public IP ${rs_pip}:"
ensure_public_ip
info "route server ${rs}:"
ensure_route_server
info ""

if [[ "${dry_run}" == true ]]; then
    info "would then read back the Route Server's addresses and ASN"
    report
    exit $?
fi

# --- what it came up as -------------------------------------------------

# Read back rather than predicted. The ASN is documented as 65515 and
# the addresses are allocated out of RouteServerSubnet, so neither is
# ours to know, and anything built against a guess fails in a way that
# looks like a BGP problem rather than like a wrong address.
# Waited for rather than asserted, so that adopting a Route Server left
# half-built by an interrupted run resumes instead of failing. That is
# what makes rerunning this the cheap way to work: a complete estate is
# adopted in seconds, and an incomplete one is waited out.
ready_rc=0
wait_until "${ready_timeout}" 20 \
    "the route server to report its addresses" route_server_ready || ready_rc=$?
case ${ready_rc} in
    0) ;;
    2) die "cannot tell whether ${rs} has finished provisioning" \
           "The error is above. Nothing was changed." ;;
    *) die "${rs} still reports no addresses after ${ready_timeout}s" \
           "A Route Server peers from a redundant pair, and one with none has" \
           "not finished provisioning. Check it and rerun:" \
           "  az network routeserver show -g ${rg} -n ${rs} -o json" ;;
esac

rs_asn="$(az_query "read the ASN of ${rs}" \
    az network routeserver show -g "${rg}" -n "${rs}" \
    --query virtualRouterAsn -o tsv)" \
    || die "cannot read back the ASN of ${rs}"
rs_ips="$(route_server_addresses)" \
    || die "cannot read back the addresses of ${rs}"

info "route server addresses:"
while read -r ip; do
    info "  ${ip}  asn=${rs_asn:-unknown}"
done < <(print_fields "${rs_ips}")

[[ "${rs_asn}" == "65515" ]] \
    || warn "Route Server ASN is ${rs_asn:-unknown}, not the documented 65515."

ok "route server estate ready"
report
