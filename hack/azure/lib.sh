# shellcheck shell=bash
#
# Azure helpers, layered over hack/lib/common.sh. Source this, do not
# run it. The AWS equivalent is hack/aws/lib.sh; nothing Azure-shaped
# belongs in common.sh.

# shellcheck source=hack/lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

# A read that failed is not an answer.
#
# Every selection these scripts make decides whether to create something
# or whether there is anything to delete, so reading a failure as
# "nothing there" either builds a second estate alongside the first or
# reports a teardown complete while the first one survives. An expired
# az login is the ordinary way that happens, and it is silent: the call
# fails, `2>/dev/null || true` supplies a plausible empty answer, and
# the script acts on it. The scripts this was ported from did exactly
# that on nearly every read.
#
# Warns and returns non-zero rather than calling die. die exits, and in
# `x="$(az_query ...)"` that exits only the substitution subshell, so
# whether the caller notices comes down to whether it happens to have
# errexit set. A helper this much depends on should not need that.
#
# stderr is kept out of the returned value, which matters more on Azure
# than on AWS: az writes upgrade notices and breaking-change warnings to
# stderr on calls that succeed, and folding them in with 2>&1 would put
# "WARNING: You have 2 update(s) available" inside a resource name that
# then goes back to Azure as --name.
az_query() {
    local what="$1"; shift
    local out err rc=0
    err="$(mktemp)"
    out="$("$@" 2>"${err}")" || rc=$?
    if (( rc != 0 )); then
        warn "cannot ${what}"
        local line
        while IFS= read -r line; do warn "  ${line}"; done <"${err}"
        rm -f "${err}"
        return 1
    fi
    rm -f "${err}"
    printf '%s' "${out}"
}

# Repeat what az said rather than naming a cause we did not observe. An
# expired refresh token, a revoked guest account and an unreachable
# login endpoint all fail this identically.
#
# The account is checked but never printed: prow logs for openshift
# repositories are public, and `az account show` carries the
# subscription id, the tenant and the signed-in user.
require_azure() {
    local err
    err="$(az account show 2>&1 >/dev/null)" && return 0
    die "cannot read the current Azure account" \
        "az said:" \
        "${err:-(az failed without saying anything)}" \
        "Log in and pick the subscription:" \
        "  az login"
}

# The cluster's identity, from the cluster, so a developer never has to
# name it and CI never has to pass it. Sets infra, rg and net_rg.
#
# No region here, unlike the AWS equivalent: Azure's infrastructure
# status does not carry one. .status.platformStatus.azure has cloudName,
# resourceGroupName and networkResourceGroupName and nothing else, so
# the region comes off the resource group instead -- see
# azure_group_location.
#
# Guarded, or errexit takes the script down before the die below can say
# anything: an unreachable cluster would exit non-zero with no
# diagnostic at all, which is the same failure this whole file is about.
azure_cluster_facts() {
    # stderr into a file rather than into the value. oc writes server
    # warnings and deprecation notices to stderr on calls that return 0,
    # and folding those in with 2>&1 leaves the warning text inside
    # infra, which still passes the -n guard below and then becomes
    # ${infra}-rs. The same rule az_query is built around.
    local err
    err="$(mktemp)"

    infra="$(oc get infrastructure cluster \
        -o jsonpath='{.status.infrastructureName}' 2>"${err}")" \
        || die "could not read the infrastructure name from the cluster" "$(<"${err}")"
    rg="$(oc get infrastructure cluster \
        -o jsonpath='{.status.platformStatus.azure.resourceGroupName}' 2>"${err}")" \
        || die "could not read the resource group from the cluster" "$(<"${err}")"
    [[ -n "${infra}" && -n "${rg}" ]] \
        || die "the cluster reported an empty infrastructure name or resource group"

    # Azure populates this even when it equals resourceGroupName, so it
    # is read rather than inferred. It differs only for a cluster
    # installed into a vnet it does not own, which is the case the
    # estate scripts have to get right: the Route Server goes in the
    # cluster's group, the subnet it needs goes in the network's.
    net_rg="$(oc get infrastructure cluster \
        -o jsonpath='{.status.platformStatus.azure.networkResourceGroupName}' 2>"${err}")" \
        || die "could not read the network resource group from the cluster" "$(<"${err}")"
    : "${net_rg:=${rg}}"

    rm -f "${err}"
}

# The subscription az is pointed at, which is not necessarily the one
# the cluster is in. The scripts compare the two rather than picking
# one, because a query against the wrong subscription answers
# confidently and wrongly.
azure_subscription() {
    az_query "read the active subscription" \
        az account show --query id -o tsv
}

# The region, from the resource group rather than from the cluster.
#
# The group is the better source anyway, since everything the estate
# scripts create goes into it and Azure requires a resource to match its
# group's location. It doubles as the check that the cluster is in the
# subscription az is pointed at, because a group in another subscription
# is not visible at all.
azure_group_location() {
    az_query "read the location of resource group $1" \
        az group show -n "$1" --query location -o tsv
}

# The cluster's virtual network, by the tag the installer puts on what
# it owns, falling back to the name it uses today.
#
# By tag rather than by name because the installer's naming has been
# stable but is not a contract, and a hardcoded name that stops matching
# reads exactly like "there is no vnet" rather than like a bug.
#
# The fallback is the half worth being careful about. A query that
# matched nothing and a query that failed must not both end in a guess:
# on a cluster whose vnet is named something else, the guess is wrong,
# and everything the estate scripts build lands somewhere nobody looks
# for it. So a failed query returns non-zero and prints nothing, and
# only a genuine empty answer falls back.
azure_cluster_vnet() {
    local group="$1" cluster="$2" tag vnet
    tag="sigs.k8s.io_cluster-api-provider-azure_cluster_${cluster}"
    vnet="$(az_query "list virtual networks in ${group}" \
        az network vnet list -g "${group}" \
        --query "[?tags.\"${tag}\"=='owned'].name | [0]" -o tsv)" || return 1
    # Azure prints the literal None for a query that matched nothing.
    [[ "${vnet}" == "None" ]] && vnet=""
    if [[ -z "${vnet}" ]]; then
        warn "no vnet in ${group} tagged ${tag}=owned;" \
             "falling back to the name ${cluster}-vnet"
        vnet="${cluster}-vnet"
    fi
    printf '%s' "${vnet}"
}
