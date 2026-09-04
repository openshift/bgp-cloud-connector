#!/usr/bin/env bash
#
# Undo hack/enable-frr.sh: take FRR and route advertisements back off the
# cluster's Network CR.
#
#   hack/disable-frr.sh --dry-run
#   hack/disable-frr.sh
#   hack/disable-frr.sh --wait-only
#
# --wait-only skips the patch and waits for the unwind somebody else
# started -- another shell, or a run of this that was interrupted after
# it patched, which otherwise leaves the cluster mid-rollout with nothing
# watching it. The refusal guards below still apply: with a BGPCloudConfiguration
# in place the operator re-patches and the unwind never completes, so
# failing on that up front beats waiting out the timeout.
#
# It gives up after 900s and prints co/network, for the reason given in
# enable-frr.sh: an external timeout(1) would kill it without saying what
# it last saw. Wrap it in `timeout` if you want a shorter bound.
#
# For the local development lifecycle. The CI job never calls this: a
# destroyed cluster unwinds it for free.
#
# This does unwind. CNO removes the frr-k8s daemonset and its namespace,
# then rolls ovnkube-node back across the cluster. Measured on 4.22.9 at
# 124s, with the reverse taking 122s.
#
# It also removes some of the CRDs, which an earlier note here claimed it
# did not. Measured across a disable on 4.22.9: bgpsessionstates and
# frrnodestates go, frrconfigurations and routeadvertisements stay. Do
# not rely on a CRD being present or absent as a signal in either
# direction.
#
# It refuses while a BGPCloudConfiguration or a RouteAdvertisements exists,
# because the operator patches the Network CR straight back during
# reconcile and you would be undoing this every thirty seconds without
# seeing why.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/lib/frr.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/lib" && pwd)/frr.sh"

timeout_secs=900
wait_only=false

for arg in "$@"; do
    case "${arg}" in
        --dry-run) dry_run=true ;;
        --wait-only) wait_only=true ;;
        *) die "unknown option: ${arg}" "Usage: ${0##*/} [--dry-run | --wait-only]" ;;
    esac
done

# Rehearsing a run that does nothing has nothing to rehearse.
if [[ "${dry_run}" == true && "${wait_only}" == true ]]; then
    die "--dry-run and --wait-only are mutually exclusive"
fi

require_cmd oc
require_cluster

# A CRD that is not installed means no such resource can exist, which is
# the ordinary case here: the operator's own CRDs are absent until
# somebody runs make install. Any other failure is a failure to find out,
# and must not read as none -- that is how a guard against an existing CR
# passes while the API is unreachable, and the patch below then fights
# the operator every thirty seconds. Both kinds are cluster scoped.
count_if_crd_present() {
    local crd="$1" kind="$2" err
    if ! err="$(oc get crd "${crd}" 2>&1 >/dev/null)"; then
        case "${err}" in
            *NotFound*) echo 0; return 0 ;;
            *) die "cannot check for ${crd}" "${err}" ;;
        esac
    fi
    oc get "${kind}" -o name | wc -l
}

configs="$(count_if_crd_present bgpcloudconfigurations.networking.openshift.io bgpcloudconfiguration)"
[[ "${configs}" == "0" ]] || die "${configs} BGPCloudConfiguration still exists" \
    "The operator re-applies the FRR patch on every reconcile, so this" \
    "would be undone immediately. Delete the CR first."

ras="$(count_if_crd_present routeadvertisements.k8s.ovn.org routeadvertisements.k8s.ovn.org)"
[[ "${ras}" == "0" ]] || die "${ras} RouteAdvertisements still exist" \
    "Remove them before disabling the feature that serves them."

info "current intent: $(frr_intent)"

if [[ "${dry_run}" == true ]]; then
    info "  would run: oc patch network.operator.openshift.io cluster --type=merge -p '${FRR_DISABLE_PATCH}'"
    info "dry run only, nothing was changed"
    exit 0
fi

if [[ "${wait_only}" == false ]]; then
    oc patch network.operator.openshift.io cluster --type=merge -p "${FRR_DISABLE_PATCH}"
else
    intent="$(frr_intent)"
    case "${intent}" in
        disabled) ;;
        unknown)  die "--wait-only, but the Network CR could not be read" \
                      "The error is above. Nothing was changed." ;;
        *)        die "--wait-only, but the Network CR asks for ${intent}" \
                      "There is no disable to watch. Run without --wait-only to start one." ;;
    esac
fi

info "waiting for CNO to remove frr-k8s and settle (up to ${timeout_secs}s)..."

# Bare, this would trip errexit before the case could read it.
rc=0
wait_until "${timeout_secs}" 10 "frr-k8s to go away" frr_disabled_done || rc=$?
case ${rc} in
    0) ;;
    2) die "gave up: the cluster is no longer heading for FRR disabled" ;;
    *) oc get co network >&2
       die "timed out after ${timeout_secs}s waiting for the unwind" ;;
esac

oc get co network
ok "frr disabled"
