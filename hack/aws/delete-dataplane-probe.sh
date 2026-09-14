#!/usr/bin/env bash
# Remove AWS data-plane probe resources.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/aws/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd aws
require_aws

if [[ -z "${INFRA:-}" ]]; then
    require_cmd oc
    require_cluster
    require_platform AWS
    aws_cluster_facts
else
    [[ -n "${AWS_REGION:-}" ]] || die "INFRA is set, so AWS_REGION must be too"
    infra="${INFRA}"
    region="${AWS_REGION}"
    export AWS_REGION="${region}" AWS_DEFAULT_REGION="${region}"
fi

name="${infra}-dataplane-probe"
instances="$(aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=${name}" \
              'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down' \
    --query 'Reservations[].Instances[].InstanceId' --output text)"
if [[ -n "${instances}" && "${instances}" != "None" ]]; then
    # shellcheck disable=SC2086
    try aws ec2 terminate-instances --instance-ids ${instances}
    if [[ "${dry_run}" == false ]]; then
        # shellcheck disable=SC2086
        aws ec2 wait instance-terminated --instance-ids ${instances}
    fi
    ok "terminated data-plane probe ${instances}"
else
    info "nothing to do: no data-plane probe instance for ${infra}"
fi

groups="$(aws ec2 describe-security-groups --filters "Name=tag:Name,Values=${name}" \
    --query 'SecurityGroups[].GroupId' --output text)"
for group in ${groups}; do
    # Remove references before deleting the probe group.
    referencing_groups="$(aws ec2 describe-security-groups \
        --filters "Name=ip-permission.group-id,Values=${group}" \
        --query 'SecurityGroups[].GroupId' --output text)"
    for referencing_group in ${referencing_groups}; do
        try aws ec2 revoke-security-group-ingress --group-id "${referencing_group}" \
            --ip-permissions "IpProtocol=tcp,FromPort=8080,ToPort=8080,UserIdGroupPairs=[{GroupId=${group}}]"
    done
    aws_retry "delete probe security group ${group}" 180 \
        aws ec2 delete-security-group --group-id "${group}"
    ok "deleted probe security group ${group}"
done

report
