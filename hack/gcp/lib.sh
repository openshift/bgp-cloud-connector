# shellcheck shell=bash
#
# GCP helpers shared by the estate scripts and the e2e entry points.
#
# The rule this file exists for is the one hack/aws/lib.sh is built
# around: a read that failed is not an answer. gcloud exits non-zero and
# says why; swallowing that with `2>/dev/null || true` turns "I could not
# ask" into "there is nothing there", which the create script acts on by
# building a second estate and the delete script acts on by reporting
# success over the first.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

# The project id is deliberately not defaulted, like the AWS account id:
# not exactly a secret, but nobody else's business, so it comes from the
# cluster or from the environment.

# gcp_query runs a read and hands back its output, or warns and fails.
#
# stderr goes to a file rather than into the value. gcloud writes
# deprecation notices and "Updated property" chatter to stderr on calls
# it answers with 0, and folding that in with 2>&1 leaves the notice
# inside a router name, which then goes back to GCP as --router.
gcp_query() {
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

# gcp_cluster_facts reads the project, region and infra id from the
# running cluster, so a developer never names them and CI never passes
# them. Sets infra, project and region.
gcp_cluster_facts() {
    # stderr into a file rather than into the value, and guarded, or
    # errexit ends the script before the die below can say anything.
    local err
    err="$(mktemp)"

    infra="$(oc get infrastructure cluster \
        -o jsonpath='{.status.infrastructureName}' 2>"${err}")" \
        || die "could not read the infrastructure name from the cluster" "$(<"${err}")"
    project="$(oc get infrastructure cluster \
        -o jsonpath='{.status.platformStatus.gcp.projectID}' 2>"${err}")" \
        || die "could not read the project from the cluster" "$(<"${err}")"
    region="$(oc get infrastructure cluster \
        -o jsonpath='{.status.platformStatus.gcp.region}' 2>"${err}")" \
        || die "could not read the region from the cluster" "$(<"${err}")"
    [[ -n "${infra}" && -n "${project}" && -n "${region}" ]] \
        || die "the cluster reported an empty infrastructure name, project or region"

    rm -f "${err}"
}

# The names the estate uses, all derived from the infra id so that two
# clusters in one project never collide.
gcp_hub_name()      { printf '%s-ncc-hub' "$1"; }
gcp_router_name()   { printf '%s-cudn-cr' "$1"; }
gcp_firewall_name() { printf '%s-bgp' "$1"; }
gcp_spoke_prefix()  { printf '%s-bgp-spoke' "$1"; }

# Existence checks are filtered lists rather than describes: a describe
# cannot tell "it is gone" from "the question failed", and both exit
# non-zero.
gcp_router_exists() {
    local out
    out="$(gcp_query "list Cloud Routers in ${2}" \
        gcloud compute routers list --project="$3" --regions="$2" \
        --filter="name=$1" --format='value(name)')" || return 2
    [[ -n "${out}" ]]
}

# The hub filter is a suffix match, not an equality one, and the two GCP
# APIs disagree about this. compute routers filter on the short name, so
# name=<name> works above. network-connectivity hubs filter on the full
# resource path, projects/<p>/locations/global/hubs/<name>, while
# --format='value(name)' prints only the last segment -- so name=<name>
# silently matches nothing and the hub reads as absent. Measured against
# a hub that was plainly there.
gcp_hub_exists() {
    local out
    out="$(gcp_query "list NCC hubs" \
        gcloud network-connectivity hubs list --project="$2" \
        --filter="name~/${1}\$" --format='value(name)')" || return 2
    [[ -n "${out}" ]]
}

# The Cloud Router's interface addresses are what the router nodes peer
# with, and what the generated profile has to describe.
#
# One per line and without the prefix length. gcloud returns a repeated
# field as one semicolon-separated value, and each entry carries the
# mask it was allocated with -- 10.0.128.5/17 -- which is not what a BGP
# neighbour address is.
gcp_router_interface_addresses() {
    local raw
    raw="$(gcp_query "read the interfaces of Cloud Router $1" \
        gcloud compute routers describe "$1" --project="$3" --region="$2" \
        --format='value(interfaces[].ipRange)')" || return 1
    printf '%s' "${raw}" | tr ';' '\n' | sed 's|/.*||' | grep .
}
