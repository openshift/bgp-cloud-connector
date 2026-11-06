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

# The e2e client outside the cluster, and the rule admitting it to the
# workers. The client's network tag is its own name.
gcp_client_name()          { printf '%s-e2e-client' "$1"; }
gcp_client_firewall_name() { printf '%s-e2e-client' "$1"; }

# The project custom role a test run grants the operator, shared by
# every cluster in the project; see gcp_ensure_custom_role. Its own id
# rather than the bgpCloudConnector docs/gcp-authentication.md suggests,
# so that a test run never changes a role an administrator made.
gcp_operator_role_id() { printf 'bgpCloudConnectorE2E'; }

# The service account the operator impersonates on a workload identity
# cluster. GCP takes 6 to 30 characters, [a-z][a-z0-9-]*[a-z0-9], and
# an infra id from a desk install is long enough to overrun that, so the
# name is cut to fit, and never left ending on a hyphen.
gcp_operator_sa_name() {
    local name="bgpcc-$1"
    name="${name:0:30}"
    while [[ "${name}" == *- ]]; do name="${name%-}"; done
    printf '%s' "${name}"
}

# A short fingerprint of a project id: the first 8 hex digits of its
# sha256. CI masks the project id in job logs, which are public, and a
# run has to say which project it leased without naming it; anyone who
# already knows the candidate projects can tell them apart by this.
gcp_project_fingerprint() {
    printf '%s' "$1" | sha256sum | cut -c1-8
}

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
    grep -Fxq -- "${want}" <<<"${out}"
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

# The spokes attached to one hub, one "<name>\t<location>" per line.
# gcloud prints both names shortened to their last segment, so the hub
# comparison is exact, for the same reason as gcp_name_exists, and the
# location is read from the spoke's path, because a delete needs it: the
# operator's spokes are regional. A list that failed returns 1 rather
# than reporting no spokes.
gcp_hub_spokes() {
    local hub="$1" project="$2" out name parent location
    out="$(gcp_query "list spokes" \
        gcloud network-connectivity spokes list --project="${project}" \
        --format='value(name.basename(),hub.basename(),name.segment(3))')" || return 1
    while IFS=$'\t' read -r name parent location; do
        if [[ -n "${name}" && "${parent}" == "${hub}" ]]; then
            printf '%s\t%s\n' "${name}" "${location}"
        fi
    done <<<"${out}"
}

gcp_firewall_exists() {
    gcp_name_exists "$1" "list firewall rules" \
        gcloud compute firewall-rules list --project="$2" \
        --format='value(name)'
}

# The zones holding an instance of this name, one per line. GCP names
# instances per zone rather than per project, so the lookup is
# project-wide: a client looked for only in the zone a run would choose
# is missed when it was built in another. A list that failed returns 1
# rather than reporting none.
gcp_instance_zones() {
    local want="$1" project="$2" out name zone
    out="$(gcp_query "list instances" \
        gcloud compute instances list --project="${project}" \
        --format='value(name,zone.basename())')" || return 1
    while IFS=$'\t' read -r name zone; do
        if [[ "${name}" == "${want}" ]]; then
            printf '%s\n' "${zone}"
        fi
    done <<<"${out}"
}

# The network and subnet the workers sit in, as "<network>\t<subnet>",
# read from the first worker's instance. Read rather than found by name:
# a cluster installed into a network it was given has names that follow
# no convention of ours. Returns 1 when there is no worker or the read
# failed, rather than reporting none.
gcp_worker_network() {
    local nodes provider project zone name
    nodes="$(oc get nodes -l node-role.kubernetes.io/worker \
        -o jsonpath='{range .items[*]}{.metadata.name} {.spec.providerID}{"\n"}{end}')" \
        || return 1
    provider="$(sort <<<"${nodes}" | awk 'NF == 2 && !seen { print $2; seen = 1 }')"
    [[ "${provider}" == gce://*/*/* ]] || return 1
    IFS=/ read -r project zone name <<<"${provider#gce://}"
    gcp_query "read worker ${name}" \
        gcloud compute instances describe "${name}" --project="${project}" --zone="${zone}" \
        --format='value(networkInterfaces[0].network.basename(),networkInterfaces[0].subnetwork.basename())'
}

# The permissions the scripts here and the e2e suite need, as IAM names
# them, including the ones gcloud uses on its own: it waits on the
# operation a create returns and reads the resource back, so a create
# needs the matching operations.get and get as well.
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
    compute.disks.create                  # create-client-vm.sh, the boot disk
    compute.firewalls.create              # create-cloud-router.sh, firewall
    compute.firewalls.delete              # delete-cloud-router.sh
    compute.firewalls.get                 # gcloud reads the rule back after creating it
    compute.firewalls.list                # gcp_firewall_exists
    compute.globalOperations.get          # gcloud waits on firewall create and delete
    compute.instances.create              # create-client-vm.sh
    compute.instances.delete              # delete-client-vm.sh
    compute.instances.get                 # gcp_worker_network; the suite, reading canIpForward and finding the client
    compute.instances.getSerialPortOutput # the e2e suite, reading a probe round
    compute.instances.list                # gcp_instance_zones, and the e2e suite
    compute.instances.setLabels           # create-client-vm.sh, labels at create
    compute.instances.setMetadata         # the startup script at create, and each probe round
    compute.instances.setTags             # create-client-vm.sh, the network tag at create
    compute.instances.update              # the suite, flipping canIpForward for the operator to repair
    compute.networks.updatePolicy         # attaching the rule and the router; the suite's peer edit
    compute.regionOperations.get          # gcloud and the suite wait on router changes
    compute.routers.create                # create-cloud-router.sh
    compute.routers.delete                # delete-cloud-router.sh
    compute.routers.get                   # write-e2e-profile.sh and the suite; gcloud reads back after add-interface
    compute.routers.list                  # gcp_router_exists
    compute.routers.update                # add-interface, twice; the suite, removing a peer for the operator to repair
    compute.subnetworks.use               # create-client-vm.sh, an address in the worker subnet
    compute.zoneOperations.get            # the suite waits on the instance update
    networkconnectivity.hubs.create       # create-cloud-router.sh
    networkconnectivity.hubs.delete       # delete-cloud-router.sh
    networkconnectivity.hubs.get          # gcloud reads the hub back after creating it
    networkconnectivity.hubs.list         # gcp_hub_exists
    networkconnectivity.operations.get    # gcloud waits on hub create and delete, and spoke delete
    networkconnectivity.spokes.delete     # delete-cloud-router.sh, spokes the operator left on the hub
    networkconnectivity.spokes.list       # delete-cloud-router.sh, before the hub goes; the suite
)

# The permissions create-operator-identity.sh and
# delete-operator-identity.sh need, as gcp_estate_permissions is for the
# estate: the CI cluster profile's account in a job and yours at a desk,
# not the operator's.
#
# Consumed by the scripts that source this, not here.
# shellcheck disable=SC2034
gcp_identity_permissions=(
    iam.roles.create                       # gcp_ensure_custom_role, the first run in a project
    iam.roles.get                          # gcp_ensure_custom_role, what the role holds
    iam.roles.list                         # gcp_ensure_custom_role, whether it exists
    iam.roles.undelete                     # gcp_ensure_custom_role, a role somebody deleted
    iam.roles.update                       # gcp_ensure_custom_role, what the operator newly needs
    iam.serviceAccounts.create             # gcp_ensure_service_account
    iam.serviceAccounts.delete             # delete-operator-identity.sh
    iam.serviceAccounts.getIamPolicy       # gcp_ensure_sa_binding, reading before writing
    iam.serviceAccounts.list               # gcp_sa_exists
    iam.serviceAccounts.setIamPolicy       # gcp_ensure_sa_binding, the workloadIdentityUser binding
    resourcemanager.projects.get           # the project number the pool's principal names
    resourcemanager.projects.getIamPolicy  # gcp_ensure_project_binding, reading before writing
    resourcemanager.projects.setIamPolicy  # the role binding on the project, added and removed
)

# The operator's own permissions, for the role a workload identity
# cluster grants the service account it impersonates. CCO cannot mint on
# such a cluster, so somebody has to make that role, and in a test run
# that is create-operator-identity.sh.
#
# A copy of the list in internal/platform/gcp/credentials.go, which is
# what the operator asks CCO for. The operator writes that request only
# once a BGPCloudConfiguration exists, after the identity has to be in
# place, so the list cannot be read from the cluster; hack/lib-test.sh
# fails if the two differ.
#
# Consumed by the scripts that source this, not here.
# shellcheck disable=SC2034
gcp_operator_permissions=(
    compute.routers.get
    compute.routers.update
    compute.networks.updatePolicy
    compute.networks.get
    compute.instances.get
    compute.instances.update
    compute.zoneOperations.get
    compute.regionOperations.get
    networkconnectivity.operations.get
    networkconnectivity.hubs.get
    networkconnectivity.spokes.get
    networkconnectivity.spokes.list
    networkconnectivity.spokes.create
    networkconnectivity.spokes.update
    networkconnectivity.spokes.delete
)

# gcp_ensure_custom_role makes sure a project custom role exists and
# holds at least the named permissions:
#
#   gcp_ensure_custom_role <project> <role id> <title> <permission>...
#
# The role is shared by every cluster in the project and never deleted,
# the way ccoctl's per-project roles and CCO's minted ones are. A
# deleted custom role keeps its place in the project's limit of 300 for
# up to two weeks and its id for a month after that, and the CI projects
# are shared with every other job, so a role per run would spend a limit
# that is not ours.
#
# Missing permissions are added and extra ones left, with a warning.
# Runs at different heads overlap in one project, and removing what this
# head no longer lists would take it from a run that still needs it. A
# role somebody deleted is undeleted, because its id cannot be created
# again until GCP has purged it.
gcp_ensure_custom_role() {
    local project="$1" role="$2" title="$3"; shift 3
    local roles id deleted found=false was_deleted=false
    roles="$(gcp_query "list custom roles" \
        gcloud iam roles list --project="${project}" --show-deleted \
        --format='value(name.basename(),deleted)')" || return 1
    while IFS=$'\t' read -r id deleted; do
        if [[ "${id}" == "${role}" ]]; then
            found=true
            [[ "${deleted}" == True ]] && was_deleted=true
        fi
    done <<<"${roles}"

    local perms
    printf -v perms '%s,' "$@"
    perms="${perms%,}"

    if [[ "${found}" == false ]]; then
        info "  creating role ${role}"
        try gcloud iam roles create "${role}" --project="${project}" \
            --title="${title}" --permissions="${perms}"
        return
    fi
    if [[ "${was_deleted}" == true ]]; then
        info "  undeleting role ${role}"
        try gcloud iam roles undelete "${role}" --project="${project}" || return 1
    fi

    local held
    held="$(gcp_query "read role ${role}" \
        gcloud iam roles describe "${role}" --project="${project}" \
        --format='value(includedPermissions)')" || return 1
    held="$(tr ';' '\n' <<<"${held}")"

    local p missing=() extra=()
    for p in "$@"; do
        grep -Fxq -- "${p}" <<<"${held}" || missing+=("${p}")
    done
    while IFS= read -r p; do
        [[ -n "${p}" ]] || continue
        printf '%s\n' "$@" | grep -Fxq -- "${p}" || extra+=("${p}")
    done <<<"${held}"
    (( ${#extra[@]} == 0 )) \
        || warn "  role ${role} also holds ${extra[*]}; left in place"

    if (( ${#missing[@]} == 0 )); then
        info "  adopting role ${role}"
        return 0
    fi
    printf -v perms '%s,' "${missing[@]}"
    info "  adding ${missing[*]} to role ${role}"
    try gcloud iam roles update "${role}" --project="${project}" \
        --add-permissions="${perms%,}"
}

# The per-cluster half of a workload identity: the service account the
# operator impersonates and its two bindings. They go when the cluster
# does, unlike the role.
#
# Only the account's name is printed, never its email or a binding's
# member: both carry the project id, and prow logs for openshift
# repositories are public.

gcp_sa_email() { printf '%s@%s.iam.gserviceaccount.com' "$1" "$2"; }

gcp_sa_exists() {
    gcp_name_exists "$(gcp_sa_email "$2" "$1")" "list service accounts" \
        gcloud iam service-accounts list --project="$1" --format='value(email)'
}

gcp_ensure_service_account() {
    local project="$1" name="$2" rc=0
    gcp_sa_exists "${project}" "${name}" || rc=$?
    case "${rc}" in
        0) info "  adopting service account ${name}" ;;
        1) info "  creating service account ${name}"
           try gcloud iam service-accounts create "${name}" --project="${project}" \
               --display-name="${name}" ;;
        *) return 1 ;;
    esac
}

gcp_delete_service_account() {
    local project="$1" name="$2" rc=0
    gcp_sa_exists "${project}" "${name}" || rc=$?
    case "${rc}" in
        0) info "  deleting service account ${name}"
           try gcloud iam service-accounts delete "$(gcp_sa_email "${name}" "${project}")" \
               --project="${project}" --quiet ;;
        1) info "  service account ${name} already gone" ;;
        *) return 1 ;;
    esac
}

# Whether a policy binds this member to this role: 0 yes, 1 no, 2 the
# policy could not be read. The policy comes from the command given,
# flattened to "<role>\t<member>" lines and compared whole.
_gcp_policy_binds() {
    local role="$1" member="$2"; shift 2
    local out
    out="$(gcp_query "read an IAM policy" "$@" \
        --flatten='bindings[].members' \
        --format='value(bindings.role,bindings.members)')" || return 2
    grep -Fxq -- "${role}"$'\t'"${member}" <<<"${out}"
}

# A policy write, retried only for what clears on its own: another
# writer changing the policy underneath this one -- other runs and
# ccoctl write the same project policy -- and a service account created
# moments ago that IAM cannot see yet. A denial fails at once.
_gcp_policy_write() {
    local attempt err rc
    err="$(mktemp)"
    for attempt in 1 2 3 4 5 6; do
        rc=0
        try "$@" 2>"${err}" || rc=$?
        if (( rc == 0 )); then
            rm -f "${err}"
            return 0
        fi
        if ! grep -Eq 'concurrent policy changes|ABORTED|does not exist' "${err}" || (( attempt == 6 )); then
            local line
            while IFS= read -r line; do warn "  ${line}"; done <"${err}"
            rm -f "${err}"
            return 1
        fi
        sleep "${RETRY_INTERVAL_SECS:-10}"
    done
}

# Binds one member to one role, or removes the binding, on whichever
# policy the read and write commands name:
#
#   _gcp_binding ensure|remove <what> <role> <member> <read...> -- <write...>
#
# A binding already in the state asked for is left alone. For a removal
# that matters beyond tidiness: gcloud refuses to remove a binding that
# is not in the policy.
_gcp_binding() {
    local mode="$1" what="$2" role="$3" member="$4" rc=0; shift 4
    local -a read=()
    while [[ "$1" != -- ]]; do read+=("$1"); shift; done; shift
    _gcp_policy_binds "${role}" "${member}" "${read[@]}" || rc=$?
    case "${mode}:${rc}" in
        ensure:0) info "  adopting ${what}" ;;
        ensure:1) info "  binding ${what}"; _gcp_policy_write "$@" ;;
        remove:0) info "  removing ${what}"; _gcp_policy_write "$@" ;;
        remove:1) info "  ${what} already gone" ;;
        *) return 1 ;;
    esac
}

# --condition=None because a project policy holding conditional bindings
# makes gcloud ask which condition is meant, and nothing here can answer.
gcp_ensure_project_binding() {
    local project="$1" role="$2" member="$3"
    _gcp_binding ensure "${role##*/} on the project" "${role}" "${member}" \
        gcloud projects get-iam-policy "${project}" -- \
        gcloud projects add-iam-policy-binding "${project}" \
        --member="${member}" --role="${role}" --condition=None
}

gcp_remove_project_binding() {
    local project="$1" role="$2" member="$3"
    _gcp_binding remove "${role##*/} on the project" "${role}" "${member}" \
        gcloud projects get-iam-policy "${project}" -- \
        gcloud projects remove-iam-policy-binding "${project}" \
        --member="${member}" --role="${role}" --condition=None
}

# The account has to exist and its policy has to be readable. A
# rehearsal never created it, so it says what it would bind and stops
# there. A real run may come straight from creating it, and IAM is
# eventually consistent: in openshift-gce-devel-ci-3 the list did not
# show an account created moments before, and elsewhere a listed
# account has answered NOT_FOUND to a read of its own policy. So a real
# run trusts neither and waits for the policy itself to answer, for up
# to GCP_IAM_WAIT_SECS.
_gcp_sa_policy_readable() {
    gcloud iam service-accounts get-iam-policy "$1" --project="$2" --format=none >/dev/null 2>&1
}

gcp_ensure_sa_binding() {
    local project="$1" email="$2" role="$3" member="$4" rc=0
    if [[ "${dry_run}" == true ]]; then
        gcp_sa_exists "${project}" "${email%%@*}" || rc=$?
        case "${rc}" in
            0) ;;
            1) info "  would bind ${role##*/} on ${email%%@*}"; return 0 ;;
            *) return 1 ;;
        esac
    else
        wait_until "${GCP_IAM_WAIT_SECS:-120}" "${RETRY_INTERVAL_SECS:-5}" \
            "IAM to see service account ${email%%@*}" \
            _gcp_sa_policy_readable "${email}" "${project}" || return 1
    fi
    _gcp_binding ensure "${role##*/} on ${email%%@*}" "${role}" "${member}" \
        gcloud iam service-accounts get-iam-policy "${email}" --project="${project}" -- \
        gcloud iam service-accounts add-iam-policy-binding "${email}" --project="${project}" \
        --member="${member}" --role="${role}"
}

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

    # The token reaches curl as a header read from a file descriptor,
    # not as an argument any local user could read from the process
    # list. printf is a builtin, so it never appears in one either.
    local answer
    answer="$(gcp_query "ask GCP which of these $# permissions the credential holds" \
        curl -fsS --max-time 30 \
        -H @<(printf 'Authorization: Bearer %s\n' "${token}") \
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
# and stops before anything is built unless it can do all of it:
#
#   gcp_require_permissions <purpose> <project> <permission>...
#
# The purpose finishes "this credential cannot ..." in the refusal.
#
# The whole list every time, and reported whether or not it passes. A
# role learned one PERMISSION_DENIED per CI run costs a cluster install
# per permission, and the run that found networkconnectivity.hubs.create
# missing could say nothing about whether anything else was.
gcp_require_permissions() {
    local purpose="$1" project="$2"; shift 2

    local held
    held="$(gcp_permissions_held "${project}" "$@")" \
        || die "could not ask GCP what this credential is allowed to do" \
               "Nothing was created."

    local p missing=()
    for p in "$@"; do
        if grep -Fxq -- "${p}" <<<"${held}"; then
            info "  granted  ${p}"
        else
            info "  DENIED   ${p}"
            missing+=("${p}")
        fi
    done

    (( ${#missing[@]} == 0 )) || die \
        "this credential cannot ${purpose}" \
        "Denied: ${missing[*]}" \
        "Nothing was created."
}

# The names of a Cloud Router's interfaces, one per line. A router with
# none answers with nothing, which is not a failure.
gcp_router_interface_names() {
    local raw
    raw="$(gcp_query "read the interfaces of Cloud Router $1" \
        gcloud compute routers describe "$1" --project="$3" --region="$2" \
        --format='value(interfaces[].name)')" || return 1
    [[ -z "${raw}" ]] || printf '%s\n' "${raw}" | tr ';' '\n'
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
    [[ -z "${raw}" ]] || printf '%s\n' "${raw}" | tr ';' '\n' | sed 's|/.*||'
}
