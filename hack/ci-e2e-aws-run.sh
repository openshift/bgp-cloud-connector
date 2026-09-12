#!/usr/bin/env bash
#
# The test half of the e2e-aws prow job. It creates and never removes;
# hack/ci-e2e-aws-teardown.sh removes and never creates; and
# hack/ci-e2e-aws.sh is the only file that knows both exist.
#
# The job definition in openshift/release names one file and nothing
# else, so everything about what the test does can be changed here, with
# a normal pull request in this repository, instead of a round trip
# through the release repo.
#
# This is the superset, not the substance. The work is done by the same
# scripts you run by hand against a cluster of your own --
# hack/enable-frr.sh and hack/aws/*.sh -- and what belongs here is only
# what CI needs and a developer does not.
#
# The steps, in order, each of them a script you can run on its own
# against a cluster of your own:
#
#   enable FRR                     hack/enable-frr.sh
#   stand up the estate            hack/aws/create-route-servers.sh
#   label the router nodes         hack/label-router-nodes.sh
#   describe what was built        hack/aws/write-e2e-profile.sh
#   create a packet probe          hack/aws/create-dataplane-probe.sh
#   run the suite                  make test-e2e-aws
#
# The order is not arbitrary. The operator discovers route servers and
# endpoints and never creates them, so the estate goes first; it selects
# nodes by label, so labelling goes before the operator looks; and the profile
# names the route server id, which does not exist until the estate does.
#
# Locally, against a cluster you already have:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig AWS_PROFILE=<profile> hack/ci-e2e-aws.sh
#
# It does not tear down, deliberately. A teardown you can run five times
# in a row and watch converge is testable in a way a trap is not, and a
# trap only ever runs in the situation nobody planned for. So this
# leaves the estate up, and either the sequencer removes it or you do:
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/ci-e2e-aws-teardown.sh

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/.." && pwd)"
# shellcheck source=hack/lib/ci.sh
source "${here}/lib/ci.sh"

ci_bootstrap

# Registered here rather than after the checks below, because
# ci_bootstrap is what creates the directory and unpacks the aws CLI
# into it, and every require_* between the two exits through die. Set
# later, a failed precheck leaves another extracted CLI behind each time.
#
# Only the scratch directory. Nothing created in the cloud is removed
# here.
trap ci_remove_workdir EXIT

require_cmd aws oc
require_cluster
require_platform AWS
require_aws

# Read once, here, and hand them to the teardown below. It can then run
# without asking the cluster anything, which matters when the reason we
# are tearing down is that the cluster stopped answering.
aws_cluster_facts

info "aws cli:  $(aws --version 2>&1)"

require_route_server_api

"${here}/enable-frr.sh"

# 65000 is the default the create script uses for the Amazon side, and
# it has to differ from the localASN the profile carries or the session
# is not eBGP. write-e2e-profile.sh checks that rather than assuming it.
"${here}/aws/create-route-servers.sh"

"${here}/label-router-nodes.sh"

# Into the scratch directory, so the suite reads a profile describing
# the estate that is actually up and the repository is left exactly as
# it was found. The checked-in profiles pin a route server id, which
# cannot work here: ours was minted a minute ago.
profile_dir="${ci_workdir}/e2e-profile"
"${here}/aws/write-e2e-profile.sh" "${profile_dir}" >/dev/null

# Create the external data-plane probe.
probe_config="${ci_workdir}/dataplane-probe.json"
"${here}/aws/create-dataplane-probe.sh" "${probe_config}"

info "--- e2e suite ---"
E2E_MANIFEST_DIR="${profile_dir}" E2E_DATAPLANE_CONFIG="${probe_config}" \
    make -C "${repo_root}" test-e2e-aws

info "e2e suite passed"
info ""
info "the estate and the manager are still up. To remove them:"
info "  INFRA=${infra} AWS_REGION=${region} hack/ci-e2e-aws-teardown.sh"
