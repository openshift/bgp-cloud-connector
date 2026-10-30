#!/usr/bin/env bash
#
# The test half of the e2e-gcp prow job. It creates and never removes;
# hack/ci-e2e-gcp-teardown.sh removes and never creates; and
# hack/ci-e2e-gcp.sh is the only file that knows both exist.
#
# The job definition in openshift/release names one file and nothing
# else, so what the test does can change here, in an ordinary pull
# request, rather than through a round trip via the release repo.
#
# The steps, in order, each a script you can run on its own:
#
#   enable FRR                     hack/enable-frr.sh
#   stand up the estate            hack/gcp/create-cloud-router.sh
#   label the router nodes         hack/label-router-nodes.sh
#   describe what was built        hack/gcp/write-e2e-profile.sh
#   run the suite                  make test-e2e-gcp
#
# The order is not arbitrary. The operator discovers the Cloud Router
# and never creates it, so the estate goes first; it selects nodes by
# label, so labelling goes before the operator looks; and the profile
# names the Cloud Router, which does not exist until the estate does.
#
# Locally, against a cluster you already have:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-gcp.sh
#
# Every step before the suite is idempotent, so rerunning this is the
# desk loop: the estate adopts in about twenty seconds against the two
# minutes it takes to build.
#
# It does not tear down, deliberately. A teardown you can run five times
# and watch converge is testable in a way a trap is not, and a trap only
# ever runs in the situation nobody planned for. So this leaves the
# estate up, and either the sequencer removes it or you do:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-gcp-teardown.sh

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/.." && pwd)"
# shellcheck source=hack/gcp/ci.sh
source "${here}/gcp/ci.sh"

ci_bootstrap

# Registered here rather than after the checks below, because
# ci_bootstrap is what creates the directory and the gcloud config lives
# in it. Only the scratch directory; nothing created in GCP is removed
# here.
trap ci_remove_workdir EXIT

require_cmd gcloud oc
require_cluster
require_platform GCP

# Read once, here, and handed to the teardown below, so it can run
# without asking the cluster anything -- which matters when the reason
# for tearing down is that the cluster stopped answering.
gcp_cluster_facts

info "gcloud: $(gcloud version 2>/dev/null | head -1)"

"${here}/enable-frr.sh"

"${here}/gcp/create-cloud-router.sh"

"${here}/label-router-nodes.sh"

# Into the scratch directory, so the suite reads a profile describing
# the estate that is actually up and the repository is left exactly as
# it was found.
profile_dir="${ci_workdir}/e2e-profile"
"${here}/gcp/write-e2e-profile.sh" "${profile_dir}" >/dev/null

info ""
info "--- estate ready ---"
info "profile written to ${profile_dir}:"
sed -e 's/^/  /' "${profile_dir}/bgpcloudconfiguration.yaml"

# The suite owns the CRs, not this script. It creates the configuration,
# the routing CR and the namespace, asserts against what the operator
# does with them, and removes all three again -- at the start of a run
# as well as the end, so a run killed by Ctrl-C or by a timeout does not
# leave the next one failing on AlreadyExists.
info "--- e2e suite ---"
E2E_MANIFEST_DIR="${profile_dir}" make -C "${repo_root}" test-e2e-gcp

info "e2e suite passed"
info ""
info "the estate is still up, and the operator is still installed. To remove the estate:"
info "  INFRA=${infra} GCP_PROJECT=${project} GCP_REGION=${region} hack/ci-e2e-gcp-teardown.sh"
