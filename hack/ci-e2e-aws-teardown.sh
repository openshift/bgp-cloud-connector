#!/usr/bin/env bash
#
# Remove whatever hack/ci-e2e-aws.sh created. Unconditional: it runs
# whether the test passed, failed, or never got as far as creating
# anything.
#
#   INFRA=<infra-id> AWS_REGION=<region> hack/ci-e2e-aws-teardown.sh
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-aws-teardown.sh
#
# Separate from the test because a trap cannot be relied on to run. Prow
# sends TERM and then, once the grace period is up, KILL -- and a killed
# shell runs no trap at all. Endpoints bill by the hour and belong to
# the VPC rather than the cluster, so nothing else reclaims them: not
# the deprovision step, and not the reaper. A step that always runs is
# the only structure that survives being killed.
#
# It must run before the cluster is deprovisioned, because the endpoints
# sit in the subnets the installer wants to delete.
#
# Safe to run when there is nothing to do, which is the normal case when
# the test tore down its own estate on the way out: it re-derives what
# is left on every run and says "nothing to do" when the answer is none.
# That is what makes it usable both from the test's trap and as a step
# of its own.

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/lib/ci.sh
source "${here}/lib/ci.sh"

attempts="${TEARDOWN_ATTEMPTS:-3}"

ci_bootstrap
trap ci_remove_workdir EXIT

require_cmd aws
require_aws

# INFRA says "you do not have to ask the cluster who it is", which is
# not the same as "there is no cluster". The caller may know the facts
# already and be handing them over precisely so that this still works if
# the cluster stops answering later. Whether there is a cluster to clean
# up is a separate question, asked below.
if [[ -z "${INFRA:-}" ]]; then
    require_cmd oc
    require_cluster
    require_platform AWS
    aws_cluster_facts
else
    [[ -n "${AWS_REGION:-}" ]] \
        || die "INFRA is set, so AWS_REGION must be too" \
               "There is no cluster left to read the region from."
    infra="${INFRA}"
    region="${AWS_REGION}"
    export AWS_DEFAULT_REGION="${region}"
fi

info "cluster:  ${infra}"
info "region:   ${region}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Everything cluster side is best-effort, and deliberately so. The
# resources that cost money are in AWS, and the whole reason this script
# exists is to remove them; letting a cluster that has stopped answering
# take the cloud teardown down with it would be the worst outcome
# available. Failures are recorded and reported at the end instead.
#
# The INFRA form exists precisely for a cluster that has gone, so there
# is nothing to ask it and nothing to clean up on it.
cluster_side_failed=false

# Reachability, not INFRA. Conflating the two skips cleanup on every run
# where the caller happened to pass the facts in, which is every CI run.
cluster_reachable=false
if command -v oc >/dev/null 2>&1 && oc whoami >/dev/null 2>&1; then
    cluster_reachable=true
fi

if [[ "${cluster_reachable}" == true ]]; then
    # Cluster side first, and specifically before the operator is
    # scaled down: both CRs carry finalizers that only the operator
    # removes, so deleting them afterwards would wait on a
    # deletionTimestamp nobody is going to clear.
    if ! "${here}/delete-e2e-crs.sh"; then
        warn "cluster-side cleanup failed; continuing to the cloud resources"
        cluster_side_failed=true
    fi

    # Then the operator. It reconciles every five minutes and recreates
    # peers it finds missing, so tearing the estate down underneath a
    # running one is a race we do not need to have. The job installs it
    # from its bundle, and a cluster where that never happened has no
    # deployment to scale, which is not a failure.
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
    if INFRA="${infra}" AWS_REGION="${region}" "${here}/aws/delete-route-servers.sh"; then
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
    "INFRA=${infra} AWS_REGION=${region} hack/aws/delete-route-servers.sh"
