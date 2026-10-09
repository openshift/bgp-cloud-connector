#!/usr/bin/env bash
#
# Stand up an EC2 instance in the cluster's VPC, outside the cluster, for
# the e2e suite to send traffic from.
#
#   hack/aws/create-client-instance.sh --dry-run
#   hack/aws/create-client-instance.sh
#
# The AWS counterpart of hack/azure/create-client-vm.sh, and there for the
# same reason: only a client outside the cluster has to use the route the
# Route Server learned to reach a pod on the advertised network, so only
# it shows whether the operator's peering and source/destination check
# settings actually carry traffic.
#
# It sits in one of the nodes' own private subnets. Inside a VPC every
# packet goes through the VPC router whichever subnet it starts in, so
# sharing a subnet with the nodes gives no shortcut to a pod; and
# create-route-servers.sh enables propagation on every route table the
# VPC has, so the learned route is already on this subnet's table, where
# a new subnet's table would not have it.
#
# Two things are created besides the instance, each tagged with the
# cluster so hack/aws/delete-client-instance.sh removes only these:
#
#   - a security group for the instance;
#   - an inbound rule on each security group the workers use, admitting
#     TCP CLIENT_PORT from that group. The installer's node group admits
#     only NodePorts from the VPC's range, so without this nothing the
#     client sends reaches a node at all.
#
# Nothing connects to the instance. Its user data installs a loop that
# reads the instance's own e2e-probe tag through the instance metadata
# service, which needs no IAM role, and whenever the round named there
# changes, requests /clientip from each address listed with it and
# prints the answers to the serial console. The suite sets the tag and
# reads the console, so driving the client takes only EC2 calls: no
# public address, no key, no agent and no inbound rule of its own.
#
# Rerunning adopts whatever already exists.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/aws/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# The port the suite's netexec pods serve on.
client_port="${CLIENT_PORT:-8080}"

# The smallest current general-purpose size. It runs curl and nothing
# else.
client_type="${CLIENT_INSTANCE_TYPE:-t3.micro}"

# Amazon Linux 2023 ships curl, which saves installing anything. Looked
# up by name rather than pinned, so the image is current in whichever
# region the cluster is in. The lookup uses EC2 rather than AWS's public
# SSM parameter because the CI account may not read SSM parameters.
ami_name="${CLIENT_AMI_NAME:-al2023-ami-2023.*-x86_64}"

# How long the client waits for one pod. A pod that is reachable answers
# in milliseconds; this only bounds the wait on one that is not.
probe_timeout="${CLIENT_PROBE_TIMEOUT:-10}"

# How long to wait for the probe loop to report itself on the console.
# Measured at under two minutes from launch.
ready_timeout="${CLIENT_READY_TIMEOUT:-600}"

parse_args "$@"
require_cmd aws oc jq
require_cluster
require_platform AWS
require_aws

# Sets infra and region, and exports AWS_REGION from the cluster.
aws_cluster_facts
vpc="$(aws_cluster_vpc)"

name="${infra}-e2e-client"
# What marks a resource as this script's, for the delete script.
owner_tag="bgp-cloud-connector-e2e-client"

info "cluster:  ${infra}"
info "region:   ${region}"
info "vpc:      ${vpc}"
info "client:   ${name}"

# The first node subnet in name order, so the choice is the same on
# every run.
subnet="$(aws_cluster_node_subnets | sort | awk 'NF {print $1; exit}')"
[[ -n "${subnet}" ]] || die "found no subnet for any node in ${vpc}"
info "subnet:   ${subnet}"

tags="{Key=Name,Value=${name}},{Key=${owner_tag},Value=${infra}}"

# --- the security group -------------------------------------------------

client_sg=""
ensure_security_group() {
    client_sg="$(aws_query "look for the security group ${name}" \
        aws ec2 describe-security-groups \
        --filters "Name=vpc-id,Values=${vpc}" "Name=group-name,Values=${name}" \
        --query 'SecurityGroups[0].GroupId' --output text)" \
        || die "cannot tell whether the security group ${name} exists"
    [[ "${client_sg}" == "None" ]] && client_sg=""
    if [[ -n "${client_sg}" ]]; then
        info "  adopting ${client_sg}"
        return 0
    fi
    if [[ "${dry_run}" == true ]]; then
        info "  would create the security group ${name} in ${vpc}"
        client_sg="<new security group>"
        return 0
    fi
    client_sg="$(aws_query "create the security group ${name}" \
        aws ec2 create-security-group --vpc-id "${vpc}" --group-name "${name}" \
        --description "bgp-cloud-connector e2e client for ${infra}" \
        --tag-specifications "ResourceType=security-group,Tags=[${tags}]" \
        --query GroupId --output text)" \
        || die "cannot create the security group ${name}"
    info "  created ${client_sg}"
}

# --- admitting the client to the nodes ------------------------------------

# Every security group on any worker, because which of them a given
# installer uses for node traffic is a naming convention rather than a
# contract. A rule on a group that does not need it admits nothing extra:
# its only source is the client's own group.
ensure_node_ingress() {
    local workers groups sg
    workers="$(oc get nodes -l node-role.kubernetes.io/worker \
        -o jsonpath='{range .items[*]}{.spec.providerID}{"\n"}{end}' | sed 's,.*/,,')"
    [[ -n "${workers}" ]] || die "no worker nodes to admit the client to"
    # shellcheck disable=SC2086 # one instance id per word is the point
    groups="$(aws_query "read the workers' security groups" \
        aws ec2 describe-instances --instance-ids ${workers} \
        --query 'Reservations[].Instances[].SecurityGroups[].GroupId' --output text)" \
        || die "cannot read the workers' security groups"

    while read -r sg; do
        [[ -n "${sg}" ]] || continue
        local existing
        existing="$(aws_query "look for the client rule on ${sg}" \
            aws ec2 describe-security-group-rules \
            --filters "Name=group-id,Values=${sg}" "Name=tag:${owner_tag},Values=${infra}" \
            --query 'length(SecurityGroupRules)' --output text)" \
            || die "cannot read the rules on ${sg}"
        if [[ "${existing}" != "0" ]]; then
            info "  ${sg} already admits the client"
            continue
        fi
        info "  admitting TCP ${client_port} from the client to ${sg}"
        try aws ec2 authorize-security-group-ingress --group-id "${sg}" \
            --ip-permissions "IpProtocol=tcp,FromPort=${client_port},ToPort=${client_port},UserIdGroupPairs=[{GroupId=${client_sg},Description=${name}}]" \
            --tag-specifications "ResourceType=security-group-rule,Tags=[{Key=${owner_tag},Value=${infra}}]"
    done < <(print_fields "${groups}" | sort -u)
}

# --- the probe loop -------------------------------------------------------

# The user data. The loop opens the console afresh for every line: a
# loop that opened it once at start printed nothing, most likely because
# agetty hangs up the serial line when it starts. It announces itself
# once a minute, so the wait below sees it whenever it started.
user_data() {
    cat <<EOF
#!/bin/bash
cat >/usr/local/bin/e2e-probe <<'PROBE'
#!/bin/bash
say() { printf '%s\n' "\$*" >/dev/console; }
imds=http://169.254.169.254/latest
last=""
n=0
while :; do
    (( n++ % 30 == 0 )) && say "e2e-probe ready"
    token="\$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' "\${imds}/api/token")"
    req="\$(curl -sf -m 2 -H "X-aws-ec2-metadata-token: \${token}" "\${imds}/meta-data/tags/instance/e2e-probe")"
    read -r round addresses <<<"\${req}"
    if [[ -n "\${round}" && "\${round}" != "\${last}" ]]; then
        for a in \${addresses}; do
            say "e2e-probe \${round} \${a} \$(curl -s -m ${probe_timeout} "http://\${a}:${client_port}/clientip")"
        done
        say "e2e-probe \${round} end"
        last="\${round}"
    fi
    sleep 2
done
PROBE
chmod +x /usr/local/bin/e2e-probe
cat >/etc/systemd/system/e2e-probe.service <<'UNIT'
[Unit]
Description=bgp-cloud-connector e2e probe
[Service]
ExecStart=/usr/local/bin/e2e-probe
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
# --no-block so cloud-init does not wait on it.
systemctl enable --now --no-block e2e-probe.service
EOF
}

# --- the instance ---------------------------------------------------------

instance=""
find_instance() {
    instance="$(aws_query "look for the instance ${name}" \
        aws ec2 describe-instances \
        --filters "Name=tag:${owner_tag},Values=${infra}" \
                  "Name=instance-state-name,Values=pending,running" \
        --query 'Reservations[0].Instances[0].InstanceId' --output text)" \
        || die "cannot tell whether the instance ${name} exists"
    [[ "${instance}" == "None" ]] && instance=""
    return 0
}

ensure_instance() {
    find_instance
    if [[ -n "${instance}" ]]; then
        info "  adopting ${instance}"
        return 0
    fi
    local ami
    # One release is published for several kernels at the same moment, so
    # the name breaks the tie on CreationDate.
    ami="$(aws_query "find the current Amazon Linux image" \
        aws ec2 describe-images --owners amazon \
            --filters "Name=name,Values=${ami_name}" "Name=state,Values=available" \
            --query "sort_by(Images, &join('', [CreationDate, Name]))[-1].ImageId" --output text)" \
        || die "cannot find an image named ${ami_name}"
    [[ "${ami}" == ami-* ]] || die "no image named ${ami_name} in ${AWS_REGION}"
    if [[ "${dry_run}" == true ]]; then
        info "  would launch ${client_type} from ${ami} in ${subnet}"
        return 0
    fi
    info "  launching ${client_type} from ${ami}"
    # InstanceMetadataTags exposes the instance's tags to the loop.
    instance="$(aws_query "launch ${name}" \
        aws ec2 run-instances --image-id "${ami}" --instance-type "${client_type}" \
        --subnet-id "${subnet}" --security-group-ids "${client_sg}" \
        --no-associate-public-ip-address \
        --metadata-options "HttpTokens=required,InstanceMetadataTags=enabled" \
        --user-data "$(user_data)" \
        --tag-specifications "ResourceType=instance,Tags=[${tags}]" \
        --query 'Instances[0].InstanceId' --output text)" \
        || die "cannot launch ${name}"
    info "  launched ${instance}"
}

# Any error reading the console is final rather than "not yet": the
# suite reads every answer from there.
probe_ready() {
    local out
    out="$(aws_query "read the console of ${instance}" \
        aws ec2 get-console-output --latest --instance-id "${instance}" \
        --query Output --output text)" || return 2
    [[ "${out}" == *"e2e-probe ready"* ]]
}

info ""
info "security group ${name}:"
ensure_security_group
info "admitting the client to the workers:"
ensure_node_ingress
info "instance ${name}:"
ensure_instance
info ""

if [[ "${dry_run}" == true ]]; then
    info "would then wait for the probe loop to report on the console"
    report
    exit $?
fi

aws ec2 wait instance-running --instance-ids "${instance}" \
    || die "${instance} did not reach running"
ready_rc=0
wait_until "${ready_timeout}" 10 "the probe loop on ${instance}" probe_ready || ready_rc=$?
case "${ready_rc}" in
    0) ;;
    2) die "cannot read the console of ${instance}" \
           "The suite reads the client's answers from its console." ;;
    *) die "the probe loop on ${instance} has not reported after ${ready_timeout}s" \
           "Its user data installs it; the console shows what cloud-init did." ;;
esac

address="$(aws_query "read the address of ${instance}" \
    aws ec2 describe-instances --instance-ids "${instance}" \
    --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)" \
    || die "cannot read back the address of ${instance}"
info "client instance: ${instance} ${address}"

ok "client instance ready"
report
