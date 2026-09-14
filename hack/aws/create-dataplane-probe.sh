#!/usr/bin/env bash
#
# Create a private VPC probe for AWS data-plane E2E tests.
#
#   hack/aws/create-dataplane-probe.sh <probe-config.json>
#
# Reruns adopt the probe tagged for this cluster.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/aws/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

port=8080
cudn_subnet="${CUDN_SUBNET:-10.100.0.0/16}"
out="${1:-}"
[[ -n "${out}" ]] || die "usage: ${0##*/} <probe-config.json>"
[[ "${cudn_subnet}" =~ ^[0-9.]+/[0-9]+$ ]] \
    || die "CUDN_SUBNET must be one IPv4 CIDR, got '${cudn_subnet}'"

require_cmd aws oc
require_cluster
require_platform AWS
require_aws
aws_cluster_facts

vpc="$(aws_cluster_vpc)"
read -r subnet az < <(aws_cluster_node_subnets | sort -k2 | head -1)
[[ -n "${subnet}" ]] || die "no cluster node subnet found for the data-plane probe"

vpc_cidr="$(aws ec2 describe-vpcs --vpc-ids "${vpc}" \
    --query 'Vpcs[0].CidrBlock' --output text)"
[[ -n "${vpc_cidr}" && "${vpc_cidr}" != "None" ]] \
    || die "the cluster VPC ${vpc} has no IPv4 CIDR"

name="${infra}-dataplane-probe"
token="bgp-cc-${infra}"

sg="$(aws ec2 describe-security-groups \
    --filters "Name=vpc-id,Values=${vpc}" "Name=tag:Name,Values=${name}" \
    --query 'SecurityGroups[0].GroupId' --output text)"
if [[ -z "${sg}" || "${sg}" == "None" ]]; then
    sg="$(aws ec2 create-security-group --vpc-id "${vpc}" \
        --group-name "${name}" --description "BGP cloud connector E2E data-plane probe" \
        --tag-specifications "ResourceType=security-group,Tags=[{Key=Name,Value=${name}},{Key=managed-by,Value=bgp-cloud-connector-e2e}]" \
        --query 'GroupId' --output text)"
    info "OK   created probe security group ${sg}"
else
    info "OK   adopting probe security group ${sg}"
fi

# Ignore duplicate rules on reruns.
authorize_cidr() {
    local cidr="$1" err rc=0
    err="$(mktemp)"
    aws ec2 authorize-security-group-ingress --group-id "${sg}" \
        --ip-permissions "IpProtocol=tcp,FromPort=${port},ToPort=${port},IpRanges=[{CidrIp=${cidr}}]" \
        >/dev/null 2>"${err}" || rc=$?
    if (( rc != 0 )) && ! grep -q InvalidPermission.Duplicate "${err}"; then
        while IFS= read -r line; do warn "  ${line}"; done <"${err}"
        rm -f "${err}"
        return "${rc}"
    fi
    rm -f "${err}"
}

authorize_cidr "${vpc_cidr}"
authorize_cidr "${cudn_subnet}"

# Permit traffic between the probe and worker security groups.
authorize_probe_to_worker() {
    local worker_sg="$1" err rc=0
    err="$(mktemp)"
    aws ec2 authorize-security-group-ingress --group-id "${worker_sg}" \
        --ip-permissions "IpProtocol=tcp,FromPort=${port},ToPort=${port},UserIdGroupPairs=[{GroupId=${sg}}]" \
        >/dev/null 2>"${err}" || rc=$?
    if (( rc != 0 )) && ! grep -q InvalidPermission.Duplicate "${err}"; then
        while IFS= read -r line; do warn "  ${line}"; done <"${err}"
        rm -f "${err}"
        return "${rc}"
    fi
    rm -f "${err}"
}

authorize_worker_to_probe() {
    local worker_sg="$1" err rc=0
    err="$(mktemp)"
    aws ec2 authorize-security-group-ingress --group-id "${sg}" \
        --ip-permissions "IpProtocol=tcp,FromPort=${port},ToPort=${port},UserIdGroupPairs=[{GroupId=${worker_sg}}]" \
        >/dev/null 2>"${err}" || rc=$?
    if (( rc != 0 )) && ! grep -q InvalidPermission.Duplicate "${err}"; then
        while IFS= read -r line; do warn "  ${line}"; done <"${err}"
        rm -f "${err}"
        return "${rc}"
    fi
    rm -f "${err}"
}

node_instances=()
for provider_id in $(oc get nodes -l node-role.kubernetes.io/worker \
    -o jsonpath='{.items[*].spec.providerID}'); do
    node_instances+=("${provider_id##*/}")
done
(( ${#node_instances[@]} > 0 )) || die "no AWS worker instances found"
worker_groups="$(aws ec2 describe-instances --instance-ids "${node_instances[@]}" \
    --query 'Reservations[].Instances[].SecurityGroups[].GroupId' --output text \
    | tr '\t' '\n' | sort -u)"
for worker_group in ${worker_groups}; do
    authorize_probe_to_worker "${worker_group}"
    authorize_worker_to_probe "${worker_group}"
done

instance="$(aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=${name}" \
              'Name=instance-state-name,Values=pending,running,stopping,stopped' \
    --query 'Reservations[].Instances[].InstanceId | [0]' --output text)"

if [[ -z "${instance}" || "${instance}" == "None" ]]; then
    node_instance="$(oc get nodes -o jsonpath='{.items[0].spec.providerID}')"
    node_instance="${node_instance##*/}"
    architecture="$(aws ec2 describe-instances --instance-ids "${node_instance}" \
        --query 'Reservations[0].Instances[0].Architecture' --output text)"
    case "${architecture}" in
        arm64) instance_type=t4g.nano ;;
        x86_64) instance_type=t3.nano ;;
        *) die "unsupported node architecture for probe AMI: ${architecture}" ;;
    esac

    image="$(aws ec2 describe-images --owners amazon \
        --filters 'Name=name,Values=al2023-ami-2023.*-kernel-6.1-*' \
                  "Name=architecture,Values=${architecture}" \
                  'Name=state,Values=available' \
        --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)"
    [[ -n "${image}" && "${image}" != "None" ]] \
        || die "no Amazon Linux 2023 ${architecture} AMI found in ${region}"

    userdata="$(mktemp)"
    trap 'rm -f "${userdata}"' EXIT
    cat >"${userdata}" <<EOF
#!/bin/bash
command -v python3 >/dev/null 2>&1 || dnf install -y python3
cat >/usr/local/bin/bgp-cc-probe.py <<'PY'
import http.server
import ipaddress
import urllib.parse
import urllib.request

TOKEN = "${token}"
CUDN = ipaddress.ip_network("${cudn_subnet}")

class Handler(http.server.BaseHTTPRequestHandler):
    def reply(self, status, body):
        body = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.headers.get("X-BGP-CC-Test-Token") != TOKEN:
            self.reply(403, "forbidden")
            return
        request = urllib.parse.urlparse(self.path)
        if request.path == "/health":
            source = ipaddress.ip_address(self.client_address[0])
            if source not in CUDN:
                self.reply(409, "source %s is outside %s" % (source, CUDN))
                return
            # Return the observed source for the no-NAT assertion.
            self.reply(200, str(source))
            return
        if request.path != "/probe":
            self.reply(404, "not found")
            return
        target = urllib.parse.parse_qs(request.query).get("target", [""])[0]
        try:
            parsed = urllib.parse.urlparse(target)
            address = ipaddress.ip_address(parsed.hostname)
            if parsed.scheme != "http" or parsed.port != ${port} or address not in CUDN:
                raise ValueError("target must be the test CUDN on port ${port}")
            with urllib.request.urlopen(target, timeout=8) as response:
                self.reply(response.status, response.read(4096).decode(errors="replace"))
        except Exception as error:
            self.reply(502, str(error))

    def log_message(self, fmt, *args):
        print(fmt % args, flush=True)

http.server.ThreadingHTTPServer(("0.0.0.0", ${port}), Handler).serve_forever()
PY
cat >/etc/systemd/system/bgp-cc-probe.service <<'UNIT'
[Unit]
After=network-online.target
[Service]
ExecStart=/usr/bin/python3 /usr/local/bin/bgp-cc-probe.py
Restart=always
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now bgp-cc-probe.service
EOF

    instance="$(aws ec2 run-instances --image-id "${image}" --instance-type "${instance_type}" \
        --subnet-id "${subnet}" --security-group-ids "${sg}" --no-associate-public-ip-address \
        --user-data "file://${userdata}" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${name}},{Key=managed-by,Value=bgp-cloud-connector-e2e}]" \
        --query 'Instances[0].InstanceId' --output text)"
    info "OK   created data-plane probe ${instance} in ${az}"
else
    state="$(aws ec2 describe-instances --instance-ids "${instance}" \
        --query 'Reservations[0].Instances[0].State.Name' --output text)"
    if [[ "${state}" == "stopped" || "${state}" == "stopping" ]]; then
        aws ec2 start-instances --instance-ids "${instance}" >/dev/null
    fi
    info "OK   adopting data-plane probe ${instance}"
fi

aws ec2 wait instance-running --instance-ids "${instance}"
ipv4="$(aws ec2 describe-instances --instance-ids "${instance}" \
    --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)"
[[ -n "${ipv4}" && "${ipv4}" != "None" ]] || die "probe ${instance} has no private IPv4 address"

mkdir -p "$(dirname "${out}")"
printf '{"ipv4":"%s","token":"%s"}\n' "${ipv4}" "${token}" >"${out}"
info "OK   data-plane probe ${instance} is ${ipv4}; config written to ${out}"
