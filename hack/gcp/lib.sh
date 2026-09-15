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

# shellcheck source=hack/lib/common.sh
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

# Existence is decided here, not by gcloud.
#
# --filter is not an identity test and is not even consistent about what
# it is. Measured on 2026-09-11 against one cluster: compute routers
# match the short name; network-connectivity hubs match the full
# resource path, projects/<p>/locations/global/hubs/<name>, while the
# value projection prints only the last segment, so name=<name> finds
# nothing and a hub that is there reads as absent; and firewall rules
# match by prefix, so name=<infra>-bgp matched <infra>-bgp-worker-subnet
# and a rule that did not exist read as present.
#
# Each of those fails in the expensive direction. An absent thing read
# as present is never created, and an existing thing read as absent is
# created twice. So the list is unfiltered on identity and the
# comparison is exact, in shell, where it means one thing.
gcp_name_exists() {
    local want="$1" what="$2"; shift 2
    local out
    out="$(gcp_query "${what}" "$@")" || return 2
    printf '%s\n' "${out}" | grep -Fxq -- "${want}"
}

gcp_router_exists() {
    gcp_name_exists "$1" "list Cloud Routers in $2" \
        gcloud compute routers list --project="$3" --regions="$2" \
        --format='value(name)'
}

gcp_hub_exists() {
    gcp_name_exists "$1" "list NCC hubs" \
        gcloud network-connectivity hubs list --project="$2" \
        --format='value(name)'
}

gcp_firewall_exists() {
    gcp_name_exists "$1" "list firewall rules" \
        gcloud compute firewall-rules list --project="$2" \
        --format='value(name)'
}

# The permissions the scripts here need, as IAM names them.
#
# This is the CI cluster profile's account in a job and yours at a desk.
# It is not the operator's: that identity is minted by the cloud
# credential operator against the list in
# internal/platform/gcp/credentials.go, and the two are granted by
# different people in different places. Confusing them costs a day.
#
# Derived from the calls below, and refined the only way a GCP role ever
# is -- by being denied. compute.networks.updatePolicy follows from no
# single call: attaching a firewall rule or a Cloud Router to a network
# counts as changing that network's policy, which is why the operator
# has to ask for it to rewrite router peers.
#
# Consumed by the scripts that source this, not here.
# shellcheck disable=SC2034
gcp_estate_permissions=(
    compute.firewalls.create        # create-cloud-router.sh, firewall
    compute.firewalls.delete        # delete-cloud-router.sh
    compute.firewalls.list          # gcp_firewall_exists
    compute.networks.list           # create-cloud-router.sh, the network read
    compute.networks.updatePolicy   # attaching the rule and the router
    compute.routers.create          # create-cloud-router.sh
    compute.routers.delete          # delete-cloud-router.sh
    compute.routers.get             # write-e2e-profile.sh, via describe
    compute.routers.list            # gcp_router_exists
    compute.routers.update          # add-interface, twice
    compute.subnetworks.list        # create-cloud-router.sh, the subnet read
    networkconnectivity.hubs.create # create-cloud-router.sh
    networkconnectivity.hubs.delete # delete-cloud-router.sh
    networkconnectivity.hubs.list   # gcp_hub_exists
    networkconnectivity.spokes.list # delete-cloud-router.sh, before the hub goes
)

# gcp_permissions_held prints which of the named permissions this
# credential actually holds on the project, one per line.
#
# testIamPermissions rather than a policy read: get-iam-policy needs
# resourcemanager.projects.getIamPolicy, which an account can itself be
# denied, and answers in roles that would have to be expanded into
# permissions here. testIamPermissions needs no permission of its own
# and answers in the same vocabulary the denials use.
#
# Over REST because gcloud has no surface for it on a project -- checked
# against 565.0.0, where `gcloud projects test-iam-permissions` is not a
# command and `gcloud iam list-testable-permissions` answers a different
# question: what may be granted on the resource, not what this caller
# holds. curl is already required by ensure-cli.sh.
gcp_permissions_held() {
    local project="$1"; shift

    local token
    token="$(gcp_query "mint an access token" \
        gcloud auth print-access-token)" || return 1

    local list
    printf -v list '"%s",' "$@"

    local answer
    answer="$(gcp_query "ask GCP which of these $# permissions the credential holds" \
        curl -fsS --max-time 30 \
        -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/json" \
        -d "{\"permissions\":[${list%,}]}" \
        "https://cloudresourcemanager.googleapis.com/v1/projects/${project}:testIamPermissions")" \
        || return 1

    # Matched as whole quoted names rather than parsed as JSON. The
    # closing quote is what stops compute.routers.get reading as held
    # because compute.routers.getIamPolicy is -- the same trap
    # gcp_name_exists avoids by comparing with grep -Fxq.
    local p
    for p in "$@"; do
        case "${answer}" in *"\"${p}\""*) printf '%s\n' "${p}" ;; esac
    done
}

# gcp_require_permissions says what this credential can and cannot do,
# and stops before anything is built unless it can do all of it.
#
# The whole list every time, and reported whether or not it passes. A
# role learned one PERMISSION_DENIED per CI run costs a cluster install
# per permission, and the run that found networkconnectivity.hubs.create
# missing could say nothing about whether anything else was.
gcp_require_permissions() {
    local project="$1"; shift

    local held
    held="$(gcp_permissions_held "${project}" "$@")" \
        || die "could not ask GCP what this credential is allowed to do" \
               "Nothing was created."

    local p missing=()
    for p in "$@"; do
        if printf '%s\n' "${held}" | grep -Fxq -- "${p}"; then
            info "  granted  ${p}"
        else
            info "  DENIED   ${p}"
            missing+=("${p}")
        fi
    done

    (( ${#missing[@]} == 0 )) || die \
        "this credential cannot build the GCP estate" \
        "Denied: ${missing[*]}" \
        "Nothing was created."
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
