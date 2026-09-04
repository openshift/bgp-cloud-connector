#!/usr/bin/env bash
#
# Remove the cluster-side objects an e2e run leaves behind: the
# BGPRouting, the BGPCloudConfiguration, and the namespace the suite
# made for the ClusterUDN.
#
#   KUBECONFIG=<cluster>/auth/kubeconfig hack/delete-e2e-crs.sh
#
# The suite creates all three and removes none of them -- it has no
# AfterAll -- and the namespace has a fixed name taken from the routing
# CR. In CI that costs nothing, because the cluster is destroyed
# afterwards. Locally it means the suite runs exactly once per cluster
# and every run after that fails on "namespaces \"prod\" already
# exists", which is a confusing way to be told the last run did not tidy
# up.
#
# Both CRs carry finalizers that the operator removes, so this has to
# run while the manager is still up. Ordering it after stopping the
# manager would hang on a deletionTimestamp nobody is going to clear.
#
# Succeeds when there is nothing to delete.

set -o nounset
set -o errexit
set -o pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=hack/lib/common.sh
source "${repo_root}/hack/lib/common.sh"

finalizer_timeout="${FINALIZER_TIMEOUT:-120}"

require_cmd oc
require_cluster

# A CRD that is not installed means no such resource can exist, which is
# the ordinary case before the CRDs go in. Any other failure is a
# failure to find out and must not read as none.
names_if_crd_present() {
    local crd="$1" kind="$2" err
    if ! err="$(oc get crd "${crd}" 2>&1 >/dev/null)"; then
        case "${err}" in
            *NotFound*) return 0 ;;
            *) die "cannot check for ${crd}" "${err}" ;;
        esac
    fi
    oc get "${kind}" -o name
}

# Deleting is asynchronous and the finalizer is the operator's to
# remove, so wait for the object to actually go rather than for the
# delete call to return.
#
# NotFound means gone. Every other failure means we could not find out,
# and answering that with "gone" reports a delete that never happened
# and skips the wait -- the same rule as names_if_crd_present above. As
# a wait_until predicate, "could not find out" is keep-waiting, so a
# broken API times out and reports a failure rather than a success.
gone() {
    local name="$1" err
    if err="$(oc get "${name}" 2>&1 >/dev/null)"; then
        return 1
    fi
    case "${err}" in
        *NotFound*) return 0 ;;
    esac
    warn "  cannot tell whether ${name} is gone: ${err}"
    return 1
}

delete_and_wait() {
    local name="$1"
    # Not guarded by errexit: the object can go between the list above
    # and this call, and a delete of something already absent is exactly
    # the outcome wanted. Aborting here would skip every later object
    # and the report that says what survived.
    if ! oc delete "${name}" --wait=false >/dev/null 2>&1; then
        if gone "${name}"; then
            ok "deleted ${name}"
            return 0
        fi
        warn "  delete ${name} was rejected; waiting to see whether it goes anyway"
    fi
    if wait_until "${finalizer_timeout}" 5 "${name} to go" gone "${name}"; then
        ok "deleted ${name}"
        return 0
    fi
    # The manager is meant to be running. When it is not -- it crashed,
    # or somebody stopped it early -- the finalizer will never clear and
    # waiting longer will not help. Clearing it by hand is safe here
    # only because the cloud resources it guards are deleted by the step
    # after this one regardless.
    warn "${name} still has a finalizer after ${finalizer_timeout}s"
    warn "  clearing it by hand; the estate is torn down separately"
    if ! oc patch "${name}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1; then
        warn "  clearing the finalizer on ${name} failed"
    fi
    if wait_until 30 2 "${name} to go after clearing its finalizer" gone "${name}"; then
        ok "deleted ${name}"
        return 0
    fi
    fail "delete ${name}"
}

# Routing first: it is the narrower object and the config is what
# describes the cloud the routing depends on.
for name in $(names_if_crd_present bgproutings.networking.openshift.io bgprouting); do
    delete_and_wait "${name}"
done

for name in $(names_if_crd_present bgpcloudconfigurations.networking.openshift.io bgpcloudconfiguration); do
    delete_and_wait "${name}"
done

# Found by label rather than by name. The suite takes the name from the
# routing CR, so a profile with a different network name makes a
# differently named namespace, and deleting a hardcoded "prod" would
# miss it while deleting somebody else's "prod" would be worse.
namespaces="$(oc get ns -l cluster-udn -o name)" \
    || die "cannot list the CUDN namespaces"

for ns in ${namespaces}; do
    if ! oc delete "${ns}" --wait=false >/dev/null 2>&1; then
        if gone "${ns}"; then
            ok "deleted ${ns}"
            continue
        fi
        warn "  delete ${ns} was rejected; waiting to see whether it goes anyway"
    fi
    if wait_until 120 5 "${ns} to go" gone "${ns}"; then
        ok "deleted ${ns}"
    else
        fail "delete ${ns}"
    fi
done

report
