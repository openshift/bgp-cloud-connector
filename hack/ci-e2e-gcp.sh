#!/usr/bin/env bash
#
# Entry point for the e2e-gcp prow job.
#
#   hack/ci-e2e-gcp.sh
#
# Right now this only asks GCP what the job's credential is allowed to
# do, and fails if the answer is not everything the estate will need.
# Nothing is created and nothing is torn down, so it needs no teardown
# half and no signal handling.
#
# It is here on its own because the answer turned out to be "it
# depends". The cluster profile leases from more than one GCP project
# and they are not equivalently permissioned: measured on 2026-09-15, a
# run in openshift-gce-devel-ci-3 could list NCC hubs and could not
# create one, so the estate died on its first resource with a single
# PERMISSION_DENIED and no account of the rest of the role. Asking for
# the whole list up front turns that into one legible report, and
# reporting it on a run that passes is what makes a green run mean
# something rather than mean the lease was lucky.
#
# The estate and the e2e suite land on top of this.

set -o nounset
set -o errexit
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/gcp/ci.sh
source "${here}/gcp/ci.sh"

ci_gcp_bootstrap
trap ci_remove_gcp_workdir EXIT

require_cmd gcloud oc
require_cluster
require_platform GCP
gcp_cluster_facts

info "cluster:  ${infra}"
info "project:  ${project}"
info "region:   ${region}"
info ""

info "permissions:"
gcp_require_permissions "${project}" "${gcp_estate_permissions[@]}"

info ""
ok "this credential can build the GCP estate"
