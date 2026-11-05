#!/usr/bin/env bash
#
# Remove what hack/gcp/create-operator-identity.sh made for one cluster.
#
#   hack/gcp/delete-operator-identity.sh --dry-run
#   hack/gcp/delete-operator-identity.sh
#
# The service account and its binding on the project. The account's own
# policy, holding the workloadIdentityUser binding, goes with it. The
# role stays: it is shared by every cluster in the project; see
# gcp_ensure_custom_role.
#
# The binding goes first. Deleting the account first would leave it in
# the project's policy as deleted:serviceAccount:..., which nobody
# removes and which counts against the policy's size.
#
# The identifiers come from the environment when the cluster has gone,
# and from the cluster otherwise, as in delete-cloud-router.sh:
#
#   INFRA=<infra id> GCP_PROJECT=<project> hack/gcp/delete-operator-identity.sh
#
# Every step runs even if an earlier one failed, and treats "already
# gone" as success, so a second run finishes what the first did not.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

parse_args "$@"
require_cmd gcloud oc

infra="${INFRA:-}"
project="${GCP_PROJECT:-}"
if [[ -z "${infra}" || -z "${project}" ]]; then
    require_cluster
    gcp_cluster_facts
fi

sa="$(gcp_operator_sa_name "${infra}")"
email="$(gcp_sa_email "${sa}" "${project}")"
role="$(gcp_operator_role_id)"

info "cluster:     ${infra}"
info "fingerprint: $(gcp_project_fingerprint "${project}") (sha256 of the project id; CI masks the id)"
info "account:     ${sa}"
info ""

info "identity:"
gcp_remove_project_binding "${project}" "projects/${project}/roles/${role}" "serviceAccount:${email}" \
    || fail "removing ${role} from ${sa} on the project"
gcp_delete_service_account "${project}" "${sa}" \
    || fail "deleting service account ${sa}"
info "  role ${role} stays; it is shared by the project"
info ""

report
