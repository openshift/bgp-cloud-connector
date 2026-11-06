#!/usr/bin/env bash
#
# Give the operator its GCP identity on a workload identity cluster.
#
#   POOL_ID=<pool> hack/gcp/create-operator-identity.sh --dry-run
#   POOL_ID=<pool> hack/gcp/create-operator-identity.sh
#
# The administrator's half of the Workload identity federation section
# of docs/gcp-authentication.md, for a test run: CCO cannot mint on a
# cluster installed with ccoctl, so somebody has to make the service
# account the operator impersonates, grant it the operator's
# permissions, let the operator's ServiceAccount impersonate it through
# the cluster's pool, and hand the operator that identity through its
# Subscription, where the console would put it.
#
# POOL_ID is the workload identity pool the cluster was installed with,
# and PROVIDER_ID its provider, defaulting to POOL_ID: `ccoctl gcp
# create-all --name=<name>` names both <name>. The operator must already
# be installed; the infra id and project come from the running cluster.
#
# The role is shared by every cluster in the project and outlives this
# one; the service account and its bindings are this cluster's, and
# hack/gcp/delete-operator-identity.sh removes them. See
# gcp_ensure_custom_role for why the role is not per cluster.
#
# Rerunning adopts what already exists, so it is safe as a check on the
# current state.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

namespace=openshift-bgp-cloud-connector
deployment=openshift-bgp-cloud-connector-controller-manager
# The ServiceAccount the manager runs as, which is what the pool sees.
service_account=openshift-bgp-cloud-connector-controller-manager

parse_args "$@"
require_cmd gcloud oc jq
require_cluster
require_platform GCP

pool="${POOL_ID:-}"
[[ -n "${pool}" ]] || die "POOL_ID is not set" \
    "Set it to the workload identity pool the cluster was installed with:" \
    "ccoctl gcp create-all --name=<name> names the pool <name>."
provider="${PROVIDER_ID:-${pool}}"

# A cluster with a long-lived credential in kube-system has CCO minting
# for the operator, and an identity made here would only compete with it.
mode="$(oc get cloudcredential cluster -o jsonpath='{.spec.credentialsMode}')" \
    || die "cannot read the cluster's credentials mode"
issuer="$(oc get authentication cluster -o jsonpath='{.spec.serviceAccountIssuer}')" \
    || die "cannot read the cluster's service account issuer"
[[ "${mode}" == Manual && -n "${issuer}" ]] || die \
    "this is not a workload identity cluster" \
    "credentialsMode is '${mode:-<default>}' and the service account issuer is '${issuer:-<unset>}'." \
    "On a cluster CCO mints for, the operator needs no identity from here."

gcp_cluster_facts

sa="$(gcp_operator_sa_name "${infra}")"
email="$(gcp_sa_email "${sa}" "${project}")"
role="$(gcp_operator_role_id)"

info "cluster:     ${infra}"
info "fingerprint: $(gcp_project_fingerprint "${project}") (sha256 of the project id; CI masks the id)"
info "pool:        ${pool}"
info "provider:    ${provider}"
info "account:     ${sa}"
info "role:        ${role} (shared by the project, never deleted)"
info ""

# Asked before anything is created, and reported in full; see
# create-cloud-router.sh.
info "permissions:"
gcp_require_permissions "give the operator its identity" "${project}" "${gcp_identity_permissions[@]}"
info ""

project_number="$(gcp_query "read the project number" \
    gcloud projects describe "${project}" --format='value(projectNumber)')" \
    || die "cannot read the project number; nothing was created"

info "identity:"
gcp_ensure_custom_role "${project}" "${role}" "bgp-cloud-connector e2e" \
    "${gcp_operator_permissions[@]}" \
    || die "could not make role ${role} hold the operator's permissions"
gcp_ensure_service_account "${project}" "${sa}" \
    || die "could not create service account ${sa}"
gcp_ensure_project_binding "${project}" "projects/${project}/roles/${role}" "serviceAccount:${email}" \
    || die "could not grant ${role} to ${sa}"
gcp_ensure_sa_binding "${project}" "${email}" roles/iam.workloadIdentityUser \
    "principal://iam.googleapis.com/projects/${project_number}/locations/global/workloadIdentityPools/${pool}/subject/system:serviceaccount:${namespace}:${service_account}" \
    || die "could not let ${namespace}/${service_account} impersonate ${sa}"
info ""

# --- the Subscription ------------------------------------------------
#
# Where the console puts the four values on an OperatorHub install, and
# where the operator reads them from: OLM copies the Subscription's env
# into the Deployment. Other variables on the Subscription are kept.
info "subscription:"
subscription="$(oc -n "${namespace}" get subscriptions.operators.coreos.com -o name)" \
    || die "cannot list Subscriptions in ${namespace}"
[[ "$(grep -c . <<<"${subscription}")" == 1 ]] \
    || die "want exactly one Subscription in ${namespace}, found: ${subscription:-none}"

env="$(oc -n "${namespace}" get "${subscription}" -o json | jq -c \
    --arg n "${project_number}" --arg p "${pool}" --arg v "${provider}" --arg e "${email}" '
    def wanted: [
        {name: "PROJECT_NUMBER", value: $n},
        {name: "POOL_ID", value: $p},
        {name: "PROVIDER_ID", value: $v},
        {name: "SERVICE_ACCOUNT_EMAIL", value: $e}];
    ((.spec.config.env // []) | map(select(.name as $x | wanted | map(.name) | index($x) | not)))
    + wanted')" || die "cannot read ${subscription}"

try oc -n "${namespace}" patch "${subscription}" --type=merge \
    -p "{\"spec\":{\"config\":{\"env\":${env}}}}" \
    || die "could not set the identity on ${subscription}"
ok "set PROJECT_NUMBER, POOL_ID, PROVIDER_ID and SERVICE_ACCOUNT_EMAIL on ${subscription#*/}"

if [[ "${dry_run}" == false ]]; then
    # OLM rewrites the Deployment from the Subscription; wait for the
    # identity to reach it, then for the new pod.
    deployment_has_identity() {
        local got
        got="$(oc -n "${namespace}" get deployment "${deployment}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env[?(@.name=="SERVICE_ACCOUNT_EMAIL")].value}')" \
            || return 1
        [[ "${got}" == "${email}" ]]
    }
    wait_until 300 5 "the identity to reach the operator's Deployment" deployment_has_identity \
        || die "the Subscription's identity never reached deployment/${deployment}"
    oc -n "${namespace}" rollout status "deployment/${deployment}" --timeout=5m \
        || die "deployment/${deployment} did not roll out with its identity"
fi

info ""
if [[ "${dry_run}" == true ]]; then
    info "Dry run only, nothing was changed."
else
    ok "the operator has its GCP identity"
fi
