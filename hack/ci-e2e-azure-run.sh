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
sed 's/^/  /' "${profile_dir}/bgpcloudconfiguration.yaml"

# There is no Azure suite to run yet, and this exits non-zero rather
# than reporting success, because a job that goes green having tested
# nothing is worse than one that is honestly red. Two things are
# missing, both tracked separately:
#
#   - test/e2e/azure does not exist. The shared suite under test/e2e
#     asserts spec.aws is nil and spec.bgp.peerGroups is non-empty, so
#     it serves platform Manual only and cannot stand in.
#
#   - the operator cannot obtain Azure credentials in a cluster.
#     buildAzurePlatform calls azureplatform.New directly, with no
#     equivalent of awsplatform.ResolveCredentials, and there is no
#     CredentialsRequest anywhere for Azure. On an IPI cluster the pod
#     cannot reach IMDS -- measured -- and there is no service account
#     issuer, so azidentity's default chain has nothing to find.
#
# Everything above this line is the part that works, and it is worth
# running on its own: it proves the estate stands up, that the profile
# describes it, and that the teardown removes it.
die "the estate is up, but there is no Azure e2e suite to run against it yet" \
    "This is the expected outcome until test/e2e/azure exists and the" \
    "operator can obtain Azure credentials in a cluster." \
    "Remove what this built with:" \
    "  INFRA=${infra} AZURE_RESOURCE_GROUP=${rg} hack/ci-e2e-azure-teardown.sh"
