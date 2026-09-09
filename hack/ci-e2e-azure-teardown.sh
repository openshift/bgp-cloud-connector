#!/usr/bin/env bash
#
# Remove whatever hack/ci-e2e-azure.sh created. Unconditional: it runs
# whether the test passed, failed, or never got as far as creating
# anything.
#
#   INFRA=<infra-id> AZURE_RESOURCE_GROUP=<group> hack/ci-e2e-azure-teardown.sh
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-azure-teardown.sh
#
# Separate from the test because a trap cannot be relied on to run. Prow
# sends TERM and then, once the grace period is up, KILL, and a killed
# shell runs no trap at all. The Route Server and its public IP bill by
# the hour. A step that always runs is the only structure that survives
# being killed.
#
# It must run before the cluster is deprovisioned. The Route Server sits
# in a subnet inside the installer's vnet, and the address prefix that
# subnet occupies was added to a vnet the installer owns, so both have
# to come out while there is still a cluster to come out of.
#
# Safe to run when there is nothing to do, which is the normal case when
# a run tore its own estate down on the way out: it re-derives what is
# left on every run and says so when the answer is none.

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/azure/ci.sh
source "${here}/azure/ci.sh"

attempts="${TEARDOWN_ATTEMPTS:-3}"

ci_bootstrap
trap ci_remove_workdir EXIT

require_cmd az
require_azure

# INFRA says "you do not have to ask the cluster who it is", which is
# not the same as "there is no cluster". The caller may know the facts
# already and be handing them over precisely so this still works if the
# cluster stops answering later. Whether there is a cluster to clean up
# is a separate question, asked below.
if [[ -z "${INFRA:-}" ]]; then
    require_cmd oc
    require_cluster
    require_platform Azure
    azure_cluster_facts
else
    [[ -n "${AZURE_RESOURCE_GROUP:-}" ]] \
        || die "INFRA is set, so AZURE_RESOURCE_GROUP must be too" \
               "There is no cluster left to read the resource group from."
    infra="${INFRA}"
    rg="${AZURE_RESOURCE_GROUP}"
    net_rg="${AZURE_NETWORK_RESOURCE_GROUP:-${rg}}"
fi

info "cluster:  ${infra}"
info "group:    ${rg}"

# Everything cluster side is best effort, and deliberately so. What
# costs money is in Azure, and the whole reason this script exists is to
# remove it; letting a cluster that has stopped answering take the cloud
# teardown down with it would be the worst outcome available. Failures
# are recorded and reported at the end instead.
cluster_side_failed=false

# Reachability, not INFRA. Conflating the two skips cleanup on every run
# where the caller happened to pass the facts in, which is every CI run.
cluster_reachable=false
if command -v oc >/dev/null 2>&1 && oc whoami >/dev/null 2>&1; then
    cluster_reachable=true
fi

if [[ "${cluster_reachable}" == true ]]; then
    # Cluster side first, and specifically before the operator is scaled
    # down: both CRs carry finalizers that only the operator removes, so
    # deleting them afterwards would wait on a deletionTimestamp nobody
    # is going to clear.
    # A longer finalizer budget than delete-e2e-crs.sh defaults to.
    # That default is 120s, which is right for AWS and much too short
    # here: measured on 2026-09-09, the operator's cleanup deletes Azure
    # Route Server peerings one at a time at about 1m33s each, so three
    # router nodes take 4m39s. At 120s the script gives up, clears the
    # finalizer by hand and reports success, and the scale-down below
    # then stops the operator part way through its own cleanup.
    #
    # Generous rather than exact, and deliberately so twice over. A
    # Route Server takes sixteen peerings, so the measurement above is a
    # floor rather than a worst case; and the measurement itself came
    # from a subscription nobody else was using, whereas CI shares its
    # quota slices. Waiting is cheaper than the alternative, which is
    # peerings left behind for the Route Server delete to collide with.
    #
    # It has to stay inside the step's grace period, though: on a
    # cancellation prow allows the whole teardown that long before it
    # sends KILL, and the Route Server delete after this measured 6m57s.
    if ! FINALIZER_TIMEOUT="${FINALIZER_TIMEOUT:-1200}" "${here}/delete-e2e-crs.sh"; then
        warn "cluster-side cleanup failed; continuing to the cloud resources"
        cluster_side_failed=true
    fi

    # Then the operator. It reconciles on a timer and recreates peerings
    # it finds missing, so tearing the estate down underneath a running
    # one is a race we do not need to have. On Azure it is worse than a
    # race: a peering that returns while the Route Server is being
    # deleted fails the delete, and the Route Server is the expensive
    # thing.
    if oc -n openshift-bgp-cloud-connector get deployment/openshift-bgp-cloud-connector-controller-manager >/dev/null 2>&1; then
        if ! oc -n openshift-bgp-cloud-connector scale deployment/openshift-bgp-cloud-connector-controller-manager --replicas=0; then
            warn "scaling the operator down failed; continuing to the cloud resources"
            cluster_side_failed=true
        fi
    else
        info "no operator deployment to scale down"
    fi
else
    info "no cluster is reachable; skipping cluster-side cleanup"
fi

# Tried more than once, which the developer-facing script does not do
# and does not need to: there a failure prints and you deal with it.
# Here nobody is watching and the resources bill by the hour, so a
# transient API failure must not be the end of it. The delete is
# idempotent and re-derives what is left on each run, so a retry costs
# a call and nothing else.
for attempt in $(seq 1 "${attempts}"); do
    if INFRA="${infra}" AZURE_RESOURCE_GROUP="${rg}" \
       AZURE_NETWORK_RESOURCE_GROUP="${net_rg}" \
       "${here}/azure/delete-route-server.sh"; then
        if [[ "${cluster_side_failed}" == true ]]; then
            die "the cloud resources are gone, but cluster-side cleanup failed" \
                "Nothing is billing. The next run against this cluster may trip" \
                "over what was left: hack/delete-e2e-crs.sh"
        fi
        info "OK   teardown complete"
        exit 0
    fi
    warn "teardown attempt ${attempt} of ${attempts} failed"
    if (( attempt < attempts )); then
        sleep 30
    fi
done

die "teardown failed after ${attempts} attempts: cloud resources are still up" \
    "Tear them down with:" \
    "INFRA=${infra} AZURE_RESOURCE_GROUP=${rg} hack/azure/delete-route-server.sh"
