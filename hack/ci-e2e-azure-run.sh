#!/usr/bin/env bash
#
# The test half of the e2e-azure prow job. It creates and never removes;
# hack/ci-e2e-azure-teardown.sh removes and never creates; and
# hack/ci-e2e-azure.sh is the only file that knows both exist.
#
# The job definition in openshift/release names one file and nothing
# else, so what the test does can change here, in an ordinary pull
# request, rather than through a round trip via the release repo.
#
# The steps, in order, each of them a script you can run on its own
# against a cluster of your own:
#
#   enable FRR                     hack/enable-frr.sh
#   stand up the estate            hack/azure/create-route-server.sh
#   label the router nodes         hack/label-router-nodes.sh
#   describe what was built        hack/azure/write-e2e-profile.sh
#
# The order is not arbitrary. The operator discovers the Route Server
# and never creates it, so the estate goes first; it selects nodes by
# label, so labelling goes before the operator looks; and the profile
# names the Route Server, which does not exist until the estate does.
#
# Locally, against a cluster you already have:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-azure.sh
#
# It does not tear down, deliberately. A teardown you can run five times
# in a row and watch converge is testable in a way a trap is not, and a
# trap only ever runs in the situation nobody planned for. So this
# leaves the estate up, and either the sequencer removes it or you do:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-azure-teardown.sh

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/azure/ci.sh
source "${here}/azure/ci.sh"

ci_bootstrap

# Registered here rather than after the checks below, because
# ci_bootstrap is what creates the directory and the az login writes its
# token cache into it. Only the scratch directory; nothing created in
# Azure is removed here.
trap ci_remove_workdir EXIT

require_cmd az oc jq
require_cluster
require_platform Azure
require_azure

# Read once, here, and handed to the teardown below, so it can run
# without asking the cluster anything -- which matters when the reason
# for tearing down is that the cluster stopped answering.
azure_cluster_facts

info "az: $(az version --query '"azure-cli"' -o tsv 2>/dev/null || echo unknown)"

"${here}/enable-frr.sh"

"${here}/azure/create-route-server.sh"

"${here}/label-router-nodes.sh"

# Into the scratch directory, so a suite reads a profile describing the
# estate that is actually up and the repository is left exactly as it
# was found.
profile_dir="${ci_workdir}/e2e-profile"
"${here}/azure/write-e2e-profile.sh" "${profile_dir}" >/dev/null

info ""
info "--- estate ready ---"
info "profile written to ${profile_dir}:"
# With the subscription id taken out. The rest of the profile is worth
# having in the log, and that one field is not: prow logs for openshift
# repositories are public.
sed -e 's/^/  /' -e 's/\(subscriptionID:\).*/\1 <redacted>/' \
    "${profile_dir}/bgpcloudconfiguration.yaml"

# Hand the estate to the operator and wait for it to say it found it.
#
# This is the one thing here that a suite would otherwise be the first
# to exercise, and it is the piece with no other cover: resolving Azure
# credentials from inside a cluster runs only where the pod cannot
# reach IMDS -- measured, curl rc 7 -- and there is no service account
# issuer, which is every IPI cluster and no unit test. Reaching
# CloudEndpointsDiscovered means the operator obtained a credential,
# authenticated, and read the Route Server this run built.
#
# Only the configuration. The BGPRouting alongside it needs a namespace
# carrying both user-defined-network labels, created with generateName,
# and none of that earns its keep until something asserts on the
# result.
info ""
info "--- operator ---"

# A configuration left mid-deletion by an earlier run is not something
# to apply over. The operator clears that finalizer, so where none is
# running the object simply stays, oc apply says "unchanged" about
# something nothing will reconcile, and the conditions on it are
# whatever the previous run left behind. Observed on a development
# cluster, where it looked exactly like success.
deletion_ts="$(oc get bgpcloudconfiguration cluster \
    -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)"
[[ -z "${deletion_ts}" ]] \
    || die "a BGPCloudConfiguration from an earlier run is still being deleted, since ${deletion_ts}" \
           "Its finalizer is the operator's, and nothing clears it while no operator is running." \
           "Start one, or remove the leftovers with hack/delete-e2e-crs.sh."

oc apply -f "${profile_dir}/bgpcloudconfiguration.yaml"

# What the operator has to be seen to have acted on. Compared below
# rather than the condition alone, because a True left by an earlier
# run satisfies a plain condition check before this configuration has
# been looked at even once -- observed, all six conditions True at
# observedGeneration 1 against a generation of 2.
generation="$(oc get bgpcloudconfiguration cluster -o jsonpath='{.metadata.generation}')"

# Reached through wait_until's "$@".
# shellcheck disable=SC2329
endpoints_discovered() {
    local status seen
    read -r status seen <<<"$(oc get bgpcloudconfiguration cluster \
        -o jsonpath="{range .status.conditions[?(@.type=='CloudEndpointsDiscovered')]}{.status} {.observedGeneration}{end}" \
        2>/dev/null)" || return 1
    [[ "${status}" == "True" && -n "${seen}" ]] || return 1
    (( seen >= generation ))
}

# Long, because this cannot report early even though discovery happens
# early. The operator writes status once, at the end of a reconcile, so
# the condition does not appear until the cloud reconcile after it has
# also finished -- measured on 10 September against a three router node
# cluster in centralus: discovery logged 1s after the apply, the three
# Route Server peerings took 3m40s, 2m21s and 2m24s, and the condition
# landed 8m50s after the apply. A 600s budget passed that run by
# seventy seconds, which is not a budget.
#
# The credential can also be missing when this starts: with none of its
# own the operator raises a CredentialsRequest and waits for the
# cloud-credential operator to mint the secret before the first
# reconcile that can discover anything.
discovery_timeout="${DISCOVERY_TIMEOUT:-1200}"

if ! wait_until "${discovery_timeout}" 10 \
        "the operator to discover the Route Server" endpoints_discovered; then
    # The status alone. The spec carries the subscription id, and prow
    # logs for openshift repositories are public.
    warn "the operator did not report the estate; its status at generation ${generation} follows"
    oc get bgpcloudconfiguration cluster -o json | jq '.status' >&2 || true
    die "the operator did not discover the Route Server within ${discovery_timeout}s" \
        "Remove what this built with:" \
        "  INFRA=${infra} AZURE_RESOURCE_GROUP=${rg} \\" \
        "    AZURE_NETWORK_RESOURCE_GROUP=${net_rg} hack/ci-e2e-azure-teardown.sh"
fi

info "the operator discovered the estate; status.peerGroups:"
oc get bgpcloudconfiguration cluster -o json | jq '.status.peerGroups'

# There is no Azure suite to run yet, and this exits non-zero rather
# than reporting success, because a job that goes green having tested
# nothing is worse than one that is honestly red. One thing is missing:
# test/e2e/azure does not exist. The shared suite under test/e2e reads
# spec.bgp.peerGroups, which the CRD requires under platform Manual and
# forbids under every cloud, so it serves Manual only and cannot stand
# in for this.
#
# Everything above this line is the part that works, and it is worth
# running on its own: it proves the estate stands up, that the profile
# describes it, that the operator can reach Azure from inside the
# cluster, and that the teardown removes what was built.
die "the estate is up and the operator discovered it, but there is no Azure e2e suite to run against it yet" \
    "This is the expected outcome until test/e2e/azure exists." \
    "Remove what this built with:" \
    "  INFRA=${infra} AZURE_RESOURCE_GROUP=${rg} \\" \
    "    AZURE_NETWORK_RESOURCE_GROUP=${net_rg} hack/ci-e2e-azure-teardown.sh"
