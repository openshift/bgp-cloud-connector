#!/usr/bin/env bash
#
# Remove what hack/aws/create-client-instance.sh built: the instance, the
# inbound rules it added to the workers' security groups, and its own
# security group.
#
#   hack/aws/delete-client-instance.sh --dry-run
#   hack/aws/delete-client-instance.sh
#
# Like hack/aws/delete-route-servers.sh it works out the cluster from
# the running cluster, or from the environment once the cluster has
# gone:
#
#   INFRA=<infra-id> AWS_REGION=<region> hack/aws/delete-client-instance.sh
#
# Everything is found by the tag the create script put on it, so nothing
# it did not create is touched. Order follows the dependencies: the
# client's security group cannot be deleted while the instance or a rule
# on another group still refers to it. Safe to run when there is nothing
# to do, and nothing stops at the first failure.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/aws/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd aws
require_aws

delete_budget="${AWS_DELETE_BUDGET:-600}"

infra="${INFRA:-}"
if [[ -z "${infra}" ]]; then
    require_cmd oc
    oc whoami >/dev/null 2>&1 \
        || die "no cluster is reachable, and INFRA was not set" \
               "Once the cluster has gone there is nothing to ask, so name it:" \
               "  INFRA=<infra-id> AWS_REGION=<region> ${0##*/}"
    require_platform AWS
    aws_cluster_facts
fi
[[ -n "${AWS_REGION:-}" ]] || die "AWS_REGION is not set and no cluster supplied it"

name="${infra}-e2e-client"
owner_tag="bgp-cloud-connector-e2e-client"

info "cluster:  ${infra}"
info "region:   ${AWS_REGION}"

# --- the instance ---------------------------------------------------------

delete_instance() {
    local ids
    ids="$(aws_query "look for the instance ${name}" \
        aws ec2 describe-instances \
        --filters "Name=tag:${owner_tag},Values=${infra}" \
                  "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query 'Reservations[].Instances[].InstanceId' --output text)" \
        || { fail "cannot tell whether the instance ${name} exists"; return 0; }
    if [[ -z "${ids}" ]]; then
        info "  already gone"
        return 0
    fi
    info "  terminating ${ids}"
    # shellcheck disable=SC2086 # one instance id per word is the point
    try aws ec2 terminate-instances --instance-ids ${ids} \
        || { fail "terminate ${ids}"; return 0; }
    if [[ "${dry_run}" != true ]]; then
        # shellcheck disable=SC2086
        aws ec2 wait instance-terminated --instance-ids ${ids} \
            || fail "${ids} did not reach terminated"
    fi
}

# --- the rules on the workers' groups -------------------------------------

delete_node_rules() {
    local rules
    rules="$(aws_query "look for the client rules" \
        aws ec2 describe-security-group-rules \
        --filters "Name=tag:${owner_tag},Values=${infra}" \
        --query 'SecurityGroupRules[].[GroupId,SecurityGroupRuleId]' --output text)" \
        || { fail "cannot read the client rules"; return 0; }
    if [[ -z "${rules}" ]]; then
        info "  none"
        return 0
    fi
    local group rule
    while read -r group rule; do
        [[ -n "${rule}" ]] || continue
        info "  removing ${rule} from ${group}"
        try aws ec2 revoke-security-group-ingress --group-id "${group}" \
            --security-group-rule-ids "${rule}" \
            || fail "remove ${rule} from ${group}"
    done <<<"${rules}"
}

# --- the client's security group ------------------------------------------

delete_security_group() {
    local sgs
    sgs="$(aws_query "look for the security group ${name}" \
        aws ec2 describe-security-groups \
        --filters "Name=tag:${owner_tag},Values=${infra}" \
        --query 'SecurityGroups[].GroupId' --output text)" \
        || { fail "cannot tell whether the security group ${name} exists"; return 0; }
    if [[ -z "${sgs}" ]]; then
        info "  already gone"
        return 0
    fi
    local sg
    for sg in ${sgs}; do
        info "  deleting ${sg}"
        if [[ "${dry_run}" == true ]]; then
            info "  would run: aws ec2 delete-security-group --group-id ${sg}"
            continue
        fi
        # A terminated instance's network interface can take a little
        # while to let go of the group, which EC2 reports as
        # DependencyViolation until it has. aws_retry retries
        # IncorrectState only, so this is its own loop.
        local deadline=$(( SECONDS + delete_budget )) out
        until out="$(aws ec2 delete-security-group --group-id "${sg}" 2>&1)"; do
            if [[ "${out}" != *DependencyViolation* ]] || (( SECONDS >= deadline )); then
                fail "delete security group ${sg}: ${out}"
                break
            fi
            sleep 10
        done
    done
}

info ""
info "instance ${name}:"
delete_instance
info "rules admitting the client to the workers:"
delete_node_rules
info "security group ${name}:"
delete_security_group
info ""

report
