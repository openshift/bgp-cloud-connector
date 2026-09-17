# shellcheck shell=bash
#
# AWS helpers, layered over hack/lib/common.sh. Source this, do not run
# it. The Azure and GCP scripts get their own equivalent; nothing
# AWS-shaped belongs in common.sh.

# shellcheck source=hack/lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"
# shellcheck source=hack/lib/retry.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/retry.sh"

# A mutating call AWS may refuse with IncorrectState until a prerequisite
# clears. Honours dry_run like try, and discards the call's output for
# the same reason.
#
# Use this rather than try for anything whose readiness depends on
# another resource. Waiting for a describe to say the prerequisite is
# clear does not work: the describe answers optimistically, so it says
# yes and the call that follows is rejected. Retrying until AWS stops
# refusing is the only signal that does not depend on what a describe
# chooses to report, and it means neither script has to know the
# dependency order exactly.
aws_retry() {
    local what="$1" budget="$2"; shift 2
    if [[ "${dry_run}" == true ]]; then
        info "  would run: $*"
        return 0
    fi
    retry_on_incorrect_state "${what}" "${budget}" "$@" >/dev/null
}

# Expired credentials and an unreachable STS endpoint fail this the same
# way and the remedy differs, so repeat what aws said rather than naming
# a cause we did not observe.
#
# The identity is checked but never printed: prow logs for openshift
# repositories are public and the ARN carries the account id and the
# principal name.
require_aws() {
    local err
    err="$(aws sts get-caller-identity 2>&1 >/dev/null)" && return 0
    die "cannot verify AWS identity" \
        "aws said:" \
        "${err:-(aws failed without saying anything)}" \
        "Select a profile, e.g. AWS_PROFILE=saml-refresh ${0##*/}" \
        "Available: $(aws configure list-profiles 2>/dev/null | tr '\n' ' ')"
}

# The infra id and region come from the running cluster, so a developer
# never has to name them and CI never has to pass them. Sets infra and
# region, and exports the region for every later aws call.
aws_cluster_facts() {
    # Guarded, or errexit takes the script down before the die below can
    # say anything: an unreachable cluster would exit non-zero with no
    # diagnostic at all, which is the same failure this whole file is
    # about.
    infra="$(oc get infrastructure cluster -o jsonpath='{.status.infrastructureName}' 2>&1)" \
        || die "could not read the infrastructure name from the cluster" "${infra}"
    region="$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.aws.region}' 2>&1)" \
        || die "could not read the region from the cluster" "${region}"
    [[ -n "${infra}" && -n "${region}" ]] \
        || die "the cluster reported an empty infrastructure name or region"
    export AWS_REGION="${region}"
    export AWS_DEFAULT_REGION="${region}"
}

# The cluster's VPC, by way of one of its nodes.
#
# Asking for the VPC tagged kubernetes.io/cluster/<infra>=owned works
# only where the installer created it. A cluster can be given a VPC
# instead, and ROSA always is: HCP takes subnet ids for a VPC you built,
# so nothing puts that tag on it and the lookup finds nothing at all.
#
# A node is in the cluster's VPC however the VPC came about, so its
# instance is the question that has an answer on both.
aws_cluster_vpc() {
    local provider_ids provider_id instance vpc

    # Guarded the way aws_cluster_facts is, and for the same two reasons.
    # A cluster we cannot reach is not a cluster without nodes, and
    # reporting it as the latter sends whoever reads the log looking in
    # the wrong place. And the callers run under errexit, so a command
    # substitution that fails ends the script where it stands: the die
    # below never speaks unless the failure is caught here.
    provider_ids="$(oc get nodes -o jsonpath='{.items[*].spec.providerID}' 2>&1)" \
        || die "could not read the nodes from the cluster" "${provider_ids}"
    # grep exits 1 when it matches nothing, and with the pipefail the
    # callers set that becomes the substitution's status, so under their
    # errexit the script would end here rather than at the die below --
    # which is the one line that says what was actually missing.
    provider_id="$(printf '%s' "${provider_ids}" | tr ' ' '\n' | grep -m1 . || true)"
    [[ -n "${provider_id}" ]] \
        || die "no node reported a providerID, so the cluster's VPC cannot be found"

    # aws:///us-east-1d/i-0f5cb92a122a30d19
    instance="${provider_id##*/}"
    vpc="$(aws ec2 describe-instances --instance-ids "${instance}" \
        --query 'Reservations[0].Instances[0].VpcId' --output text 2>&1)" \
        || die "cannot describe ${instance}, the instance behind a cluster node" "${vpc}"
    [[ "${vpc}" != "None" && -n "${vpc}" ]] \
        || die "EC2 reports no VPC for ${instance}, the instance behind a cluster node"
    printf '%s' "${vpc}"
}

# A subnet and its availability zone for every node, one line each.
#
# Asking EC2 for the subnets tagged Name=*private* works only where
# whoever built the VPC named them that way. The terraform in rosa-bgp
# does; the CloudFormation ci-operator provisions with does not tag
# subnets at all, so the filter matched nothing and create-route-servers
# stopped having done nothing, on a VPC that had a private subnet in
# every zone.
#
# A node is in the subnet its router runs in, which is where an endpoint
# belongs, and that is true of a VPC however it was built and whatever it
# named things. Zones repeat when zones have several nodes; the caller
# takes one subnet per zone.
aws_cluster_node_subnets() {
    local provider_ids out
    local -a instances=()

    provider_ids="$(oc get nodes -o jsonpath='{.items[*].spec.providerID}' 2>&1)" \
        || die "could not read the nodes from the cluster" "${provider_ids}"

    # aws:///us-west-2a/i-081c16e05f2b3f713
    local id
    while read -r id; do
        [[ -n "${id}" ]] && instances+=("${id##*/}")
    done < <(printf '%s\n' "${provider_ids}" | tr ' ' '\n')

    (( ${#instances[@]} > 0 )) \
        || die "no node reported a providerID, so the cluster's subnets cannot be found"

    out="$(aws ec2 describe-instances --instance-ids "${instances[@]}" \
        --query 'Reservations[].Instances[].[SubnetId,Placement.AvailabilityZone]' \
        --output text 2>&1)" \
        || die "cannot describe the instances behind the cluster's nodes" "${out}"
    # Terminated, so that a caller reading this with `while read` sees the
    # last line rather than assigning it and leaving the loop.
    printf '%s\n' "${out}"
}

# Ask before creating anything. An API the region does not offer and an
# API this account may not call are different answers, and a failed
# create cannot tell them apart. The error text is the distinction.
require_route_server_api() {
    local err
    err="$(aws ec2 describe-route-servers 2>&1 >/dev/null)" && return 0
    die "VPC Route Server API unavailable in ${region} for this account" "${err}"
}

# A describe that fails is not an empty answer.
#
# Every selection below decides whether to create something or whether
# there is anything to delete, so reading a failure as "nothing there"
# either builds a second estate alongside the first or reports a
# teardown complete while the first one survives. Expired credentials
# mid-run are the ordinary way this happens, and they are silent: the
# call fails, the fallback supplies a plausible answer, and the script
# acts on it.
# Warns and returns non-zero rather than calling die. die exits, and in
# `x="$(aws_query ...)"` that exits only the substitution subshell, so
# whether the caller notices comes down to whether it happens to have
# errexit set. A helper this much depends on should not need that.
#
# stderr is kept out of the returned value. Folding it in with 2>&1
# would put any warning the CLI writes while still exiting 0 into the
# route server id or the endpoint count, which then goes back to AWS as
# --route-server-id.
aws_query() {
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

# The live route server for a cluster, or the empty string if there is
# none. Tag filter server-side, state filter client-side: a deleted
# route server keeps its tag and keeps being returned, so matching on
# the tag alone adopts a tombstone, and every call against it then fails
# with IncorrectState.
route_server_for_cluster() {
    local rs
    # || return, here and in every helper below: the failure has to
    # cross the command substitution, and nothing else carries it.
    rs="$(aws_query "list route servers tagged $1-rs" \
        aws ec2 describe-route-servers \
        --filters "Name=tag:Name,Values=$1-rs" \
        --query "RouteServers[?State!='deleted' && State!='deleting'].RouteServerId | [0]" \
        --output text)" || return 1
    [[ "${rs}" == "None" ]] && rs=""
    printf '%s' "${rs}"
}

# How many live endpoints a route server has in a subnet.
#
# DescribeRouteServerEndpoints rejects route-server-id and subnet-id as
# filters, so the selection happens client-side. Counted rather than
# matched by name: the endpoints in a subnet are interchangeable, so how
# many there are is the only question worth asking, and it makes a rerun
# after a partial failure top up rather than duplicate.
live_endpoints_in_subnet() {
    local n
    n="$(aws_query "count endpoints in $2" \
        aws ec2 describe-route-server-endpoints \
        --query "length(RouteServerEndpoints[?RouteServerId=='$1' && SubnetId=='$2' && State!='deleted' && State!='deleting'])" \
        --output text)" || return 1
    printf '%s' "${n}"
}

# Every route table in a VPC.
vpc_route_tables() {
    local out
    out="$(aws_query "list route tables in $1" \
        aws ec2 describe-route-tables --filters "Name=vpc-id,Values=$1" \
        --query 'RouteTables[].RouteTableId' --output text)" || return 1
    print_fields "${out}"
}

# The route tables a route server still propagates to.
route_server_propagations() {
    local out
    out="$(aws_query "read propagations for $1" \
        aws ec2 get-route-server-propagations --route-server-id "$1" \
        --query "RouteServerPropagations[?State!='deleted' && State!='deleting'].RouteTableId" \
        --output text)" || return 1
    print_fields "${out}"
}

# The live peers on an endpoint. The operator creates these, which is
# why they exist even though nothing here made any.
#
# Everything not already dead, rather than only what is available. What
# this lists is what the teardown deletes, so a state left out is a
# resource nothing ever tries to remove -- and a peer that failed to
# come up still holds its endpoint open. Deleting one that is not ready
# is answered with IncorrectState, which the waits before the deletes
# are there to avoid and the retry around them absorbs; not listing it
# at all leaves it running with nothing to say so.
endpoint_peers() {
    local out
    out="$(aws_query "list peers on $1" \
        aws ec2 describe-route-server-peers \
        --query "RouteServerPeers[?RouteServerEndpointId=='$1' && State!='deleted' && State!='deleting'].RouteServerPeerId" \
        --output text)" || return 1
    printf '%s' "${out}"
}

# The endpoints on a route server that are not already gone. Same rule
# as the peers above: an endpoint stuck in failed keeps its ENI in a
# subnet the installer wants to delete, so the teardown has to see it.
route_server_endpoints() {
    local out
    out="$(aws_query "list endpoints on $1" \
        aws ec2 describe-route-server-endpoints \
        --query "RouteServerEndpoints[?RouteServerId=='$1' && State!='deleted' && State!='deleting'].RouteServerEndpointId" \
        --output text)" || return 1
    printf '%s' "${out}"
}

# The VPCs a route server is associated with.
#
# AWS reports "this route server has no associations" as an error rather
# than as an empty list:
#
#   InvalidRouteServerId.NotAssociated: Route Server rs-... is not
#   associated with a VPC.
#
# So an error here carries two different answers and has to be read
# rather than swallowed, or "none" and "I could not find out" become the
# same thing again.
# Not aws_query, because that reports every failure as one. The two
# streams are kept apart for the reason given there: a warning the CLI
# writes on a call it goes on to answer would otherwise become part of
# the id, and the id goes back to AWS as --vpc-id.
route_server_vpcs() {
    local out err rc=0
    err="$(mktemp)"
    out="$(aws ec2 get-route-server-associations --route-server-id "$1" \
        --query 'RouteServerAssociations[].VpcId' --output text 2>"${err}")" || rc=$?
    if (( rc == 0 )); then
        rm -f "${err}"
        printf '%s' "${out}"
        return 0
    fi
    local reason
    reason="$(cat "${err}")"
    rm -f "${err}"
    case "${reason}" in
        *NotAssociated*) return 0 ;;
    esac
    warn "cannot read associations for $1"
    local line
    while IFS= read -r line; do warn "  ${line}"; done <<<"${reason}"
    return 1
}
