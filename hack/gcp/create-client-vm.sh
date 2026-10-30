#!/usr/bin/env bash
#
# Create a VM outside the cluster that the GCP e2e suite sends traffic
# from, so that a pass depends on the Cloud Router's learned route and
# the router nodes' canIpForward rather than on the cluster's own
# network.
#
#   hack/gcp/create-client-vm.sh --dry-run
#   hack/gcp/create-client-vm.sh
#
# Needs credentials for the project owning the cluster and a KUBECONFIG:
# the infra id, project and region come from the running cluster.
#
# It sits in the workers' subnet, in the zone of the first worker by
# name. The Cloud Router programs the route it learns into the VPC, and
# the VPC routes every packet, so the subnet is a place to put the VM
# rather than a path the traffic takes.
#
# Two things are created, both named from the infra id so that
# hack/gcp/delete-client-vm.sh removes only these:
#
#   - the VM, network-tagged and labelled with its own name;
#   - a firewall rule admitting TCP CLIENT_PORT from that tag to the
#     workers' tag. The installer's rules admit node ports and the
#     cluster's own protocols between nodes, and SSH and ICMP from the
#     machine network, so without it nothing the client sends reaches a
#     pod at all.
#
# Nothing connects to the VM. Its startup script installs a loop that
# reads the VM's own e2e-probe metadata attribute from the metadata
# server and, whenever the round named there changes, requests /clientip
# from each address listed with it and prints the answers to the serial
# console. The suite sets the attribute and reads the console, so
# driving the client takes only Compute API calls: no external address,
# no key, no service account and no inbound rule of its own.
#
# Rerunning adopts whatever already exists.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# The port the suite's netexec pods serve on.
client_port="${CLIENT_PORT:-8080}"

# The smallest shared-core size. It runs curl and nothing else.
client_type="${CLIENT_MACHINE_TYPE:-e2-small}"

# Debian ships curl, which saves installing anything. Named by family so
# the image is current wherever the cluster is.
image_family="${CLIENT_IMAGE_FAMILY:-debian-12}"
image_project="${CLIENT_IMAGE_PROJECT:-debian-cloud}"

# How long the client waits for one pod. A pod that is reachable answers
# in milliseconds; this only bounds the wait on one that is not.
probe_timeout="${CLIENT_PROBE_TIMEOUT:-10}"

# How long to wait for the probe loop to report itself on the console.
ready_timeout="${CLIENT_READY_TIMEOUT:-600}"

parse_args "$@"
require_cmd gcloud oc
require_cluster
require_platform GCP

gcp_cluster_facts

name="$(gcp_client_name "${infra}")"
firewall="$(gcp_client_firewall_name "${infra}")"

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info "client:   ${name}"
info ""

info "permissions:"
gcp_require_permissions "${project}" "${gcp_estate_permissions[@]}"
info ""

network="$(gcp_query "read the cluster's network" \
    gcloud compute networks list --project="${project}" \
    --filter="name~^${infra}" --format='value(name)' | head -1)"
[[ -n "${network}" ]] || die "no network found for ${infra} in ${project}"

subnet="$(gcp_query "read the cluster's worker subnet" \
    gcloud compute networks subnets list --project="${project}" \
    --regions="${region}" --filter="name~^${infra}.*worker" \
    --format='value(name)' | head -1)"
[[ -n "${subnet}" ]] || die "no worker subnet found for ${infra} in ${region}"

# The first worker by name, so the choice is the same on every run.
zone="$(oc get nodes -l node-role.kubernetes.io/worker \
    -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' \
    | sort | awk 'NF == 2 {print $2; exit}')"
[[ -n "${zone}" ]] || die "found no worker with a topology.kubernetes.io/zone label"

info "network:  ${network}"
info "subnet:   ${subnet}"
info "zone:     ${zone}"
info ""

# --- admitting the client to the workers ---------------------------
info "firewall ${firewall}:"
rc=0
gcp_firewall_exists "${firewall}" "${project}" || rc=$?
case "${rc}" in
    0) info "  adopting the rule that is already there" ;;
    1) try gcloud compute firewall-rules create "${firewall}" \
            --project="${project}" --network="${network}" \
            --direction=INGRESS --action=allow --rules="tcp:${client_port}" \
            --source-tags="${name}" --target-tags="${infra}-worker" \
            --description="bgp-cloud-connector e2e client for ${infra}" ;;
    *) die "could not tell whether firewall rule ${firewall} exists, so nothing was created" ;;
esac

# --- the probe loop -------------------------------------------------
#
# The startup script. The loop opens the console afresh for every line,
# as the AWS client's does, and announces itself once a minute, so the
# wait below sees it whenever it started. A missing attribute reads as
# 404 and an empty request, which is no round.
startup_script() {
    cat <<EOF
#!/bin/bash
cat >/usr/local/bin/e2e-probe <<'PROBE'
#!/bin/bash
say() { printf '%s\n' "\$*" >/dev/console; }
attr=http://metadata.google.internal/computeMetadata/v1/instance/attributes/e2e-probe
last=""
n=0
while :; do
    (( n++ % 30 == 0 )) && say "e2e-probe ready"
    req="\$(curl -sf -m 2 -H 'Metadata-Flavor: Google' "\${attr}")"
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
systemctl enable --now --no-block e2e-probe.service
EOF
}

# --- the VM ---------------------------------------------------------
info "VM ${name}:"
rc=0
gcp_instance_exists "${name}" "${zone}" "${project}" || rc=$?
case "${rc}" in
    0) info "  adopting the VM that is already there" ;;
    1)
        script="$(mktemp)"
        trap 'rm -f "${script}"' EXIT
        startup_script >"${script}"
        try gcloud compute instances create "${name}" \
            --project="${project}" --zone="${zone}" \
            --machine-type="${client_type}" \
            --subnet="${subnet}" --no-address \
            --no-service-account --no-scopes \
            --image-family="${image_family}" --image-project="${image_project}" \
            --tags="${name}" \
            --labels="bgp-cloud-connector-e2e-client=${infra}" \
            --metadata-from-file=startup-script="${script}"
        ;;
    *) die "could not tell whether VM ${name} exists, so nothing was created" ;;
esac

if [[ "${dry_run}" == true ]]; then
    info ""
    info "Dry run only, nothing was changed."
    exit 0
fi

# --- ready ----------------------------------------------------------
probe_ready() {
    gcloud compute instances get-serial-port-output "${name}" \
        --project="${project}" --zone="${zone}" --port=1 2>/dev/null \
        | grep -q 'e2e-probe ready'
}
info "waiting up to ${ready_timeout}s for the probe loop on the serial console"
wait_until "${ready_timeout}" 10 "the probe loop on ${name}" probe_ready \
    || die "the probe loop on ${name} did not report on the serial console within ${ready_timeout}s"

address="$(gcp_query "read the client's address" \
    gcloud compute instances describe "${name}" \
    --project="${project}" --zone="${zone}" \
    --format='value(networkInterfaces[0].networkIP)')"

info ""
ok "client ${name} at ${address} in ${zone}"
