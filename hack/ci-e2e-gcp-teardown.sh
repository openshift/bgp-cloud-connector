#!/usr/bin/env bash
#
# The teardown half of the e2e-gcp prow job. It removes and never
# creates.
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-gcp-teardown.sh
#   INFRA=<id> GCP_PROJECT=<p> GCP_REGION=<r> hack/ci-e2e-gcp-teardown.sh
#
# Safe to run when there is nothing there: every step treats "already
# gone" as success, so this is what you reach for when you are not sure
# what a failed run left behind.
#
# The identifiers come from the environment when they are set, because a
# teardown has to work after the cluster it is tearing down stopped
# answering. The sequencer reads them once, before anything is built,
# and passes them in for exactly that reason.

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/gcp/ci.sh
source "${here}/gcp/ci.sh"

ci_bootstrap
trap ci_remove_workdir EXIT

require_cmd gcloud

infra="${INFRA:-}"
project="${GCP_PROJECT:-}"
region="${GCP_REGION:-}"
if [[ -z "${infra}" || -z "${project}" || -z "${region}" ]]; then
    require_cluster
    gcp_cluster_facts
fi

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info ""

# The CRs first, and with a budget. The operator removes the spoke and
# the peers when the configuration goes, and it has to be running to
# clear the finalizers, so this runs before anything scales it down.
#
# GCP is quicker here than Azure, where each peering is minutes: the
# spoke delete dominates and measured about eighty seconds. The budget
# is still generous, because giving up half way leaves a finalizer
# somebody has to clear by hand.
if oc get namespace >/dev/null 2>&1; then
    if ! FINALIZER_TIMEOUT="${FINALIZER_TIMEOUT:-600}" "${here}/delete-e2e-crs.sh"; then
        warn "removing the custom resources failed; the estate delete below may find a spoke still attached"
    fi
else
    info "no cluster is reachable; skipping cluster-side cleanup"
fi

INFRA="${infra}" GCP_PROJECT="${project}" GCP_REGION="${region}" \
    "${here}/gcp/delete-cloud-router.sh"

info ""
ok "teardown complete"
