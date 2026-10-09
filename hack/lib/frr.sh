# shellcheck shell=bash
#
# What enable-frr.sh and disable-frr.sh both need. Source this, do not
# run it. Nothing here is specific to any cloud: FRR is enabled the same
# way on AWS, Azure and GCP, and these scripts never learn which they
# are on.

# shellcheck source=hack/lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

# Consumed by the scripts that source this, not here.
# shellcheck disable=SC2034
FRR_ENABLE_PATCH='{"spec":{"additionalRoutingCapabilities":{"providers":["FRR"]},"defaultNetwork":{"ovnKubernetesConfig":{"routeAdvertisements":"Enabled"}}}}'
# shellcheck disable=SC2034
FRR_DISABLE_PATCH='{"spec":{"additionalRoutingCapabilities":null,"defaultNetwork":{"ovnKubernetesConfig":{"routeAdvertisements":"Disabled"}}}}'

# What the Network CR is currently asking for: enabled, disabled, mixed
# if the two fields disagree, or unknown if we could not find out.
#
# Total, and always succeeds: every outcome including the failure has a
# name in the returned value, so callers match on that rather than on a
# status that would not survive the command substitution anyway.
#
# unknown exists because the alternative is worse than useless. Both
# fields are empty on a disabled cluster, and a failed oc call also
# yields empty, so without this a cluster we cannot reach at all reads
# as a cluster that is deliberately disabled -- and every caller below
# then acts on it. A failed read is a failed poll, never an answer.
#
# mixed is nobody's intended state and worth refusing rather than
# guessing at.
frr_intent() {
    local caps ra
    if ! caps="$(oc get network.operator.openshift.io cluster \
        -o jsonpath='{.spec.additionalRoutingCapabilities}' 2>&1)"; then
        warn "  cannot read the Network CR: ${caps}"
        echo unknown
        return 0
    fi
    if ! ra="$(oc get network.operator.openshift.io cluster \
        -o jsonpath='{.spec.defaultNetwork.ovnKubernetesConfig.routeAdvertisements}' 2>&1)"; then
        warn "  cannot read the Network CR: ${ra}"
        echo unknown
        return 0
    fi

    if [[ "${caps}" == *FRR* && "${ra}" == "Enabled" ]]; then
        echo enabled
    elif [[ -z "${caps}" && "${ra}" != "Enabled" ]]; then
        echo disabled
    else
        echo mixed
    fi
}

network_degraded() {
    [[ "$(oc get co network -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}')" == "True" ]]
}

network_settled() {
    [[ "$(oc get co network -o jsonpath='{.status.conditions[?(@.type=="Progressing")].status}')" == "False" ]]
}

# CNO renders route advertisements into ovnkube-node's start script, and
# the changed script is what rolls ovnkube-node. Until it says true, a
# fully rolled-out ovnkube-node is still the one from before the patch.
ovnkube_route_advertisements_rendered() {
    local script
    script="$(oc get configmap -n openshift-ovn-kubernetes ovnkube-script-lib \
        -o jsonpath='{.data.ovnkube-lib\.sh}' 2>/dev/null)" || return 1
    grep -B1 -F 'route_advertisements_enable_flag="--enable-route-advertisements"' <<<"${script}" \
        | grep -qF '"true" == "true"'
}

# Every ovnkube-node pod is from the daemonset's current generation and
# available, and the daemonset controller has seen that generation.
ovnkube_node_rolled_out() {
    local fields gen observed desired updated available
    fields="$(oc get ds -n openshift-ovn-kubernetes ovnkube-node \
        -o jsonpath='{.metadata.generation} {.status.observedGeneration} {.status.desiredNumberScheduled} {.status.updatedNumberScheduled} {.status.numberAvailable}' 2>/dev/null)" \
        || return 1
    read -r gen observed desired updated available <<<"${fields}"
    [[ -n "${desired}" && "${desired}" != "0" && "${gen}" == "${observed}" \
        && "${updated}" == "${desired}" && "${available}" == "${desired}" ]]
}

# One field of the frr-k8s daemonset. Prints nothing and succeeds when
# the daemonset does not exist yet, which is the ordinary state during
# an enable; returns non-zero when the read itself failed, which is not
# the same thing and must not read as "not ready yet" forever.
frr_daemonset() {
    local field="$1" out
    if out="$(oc get ds -n openshift-frr-k8s frr-k8s \
        -o jsonpath="{.status.${field}}" 2>&1)"; then
        printf '%s' "${out}"
        return 0
    fi
    case "${out}" in
        *NotFound*) return 0 ;;
    esac
    warn "  cannot read the frr-k8s daemonset: ${out}"
    return 1
}

# Whether the frr-k8s namespace is there. Three answers rather than two:
#
#   0  present
#   1  absent
#   2  the read failed, so we do not know
#
# The third has to be its own answer. frr_disabled_done finishes on
# absent, and a read that failed is not a namespace that has gone -- it
# is the same false success frr_daemonset above is shaped to avoid. A
# cluster that has stopped answering altogether is already caught by the
# intent read in frr_wait_guard; this covers the narrower case where the
# Network CR is readable and this is not.
frr_namespace_present() {
    local out
    if out="$(oc get ns openshift-frr-k8s 2>&1)"; then
        return 0
    fi
    case "${out}" in
        *NotFound*) return 1 ;;
    esac
    warn "  cannot read the frr-k8s namespace: ${out}"
    return 2
}

# Shared by both predicates below. Returns 0 to carry on waiting and 2 --
# never going to become true -- when the Network CR stops asking for what
# we are waiting for. Zero means "no objection", not "done"; the caller
# decides that.
#
# Without this, a wait can be satisfied by a state it never should have
# accepted, because both scripts finish on conditions that are also true
# in the instant after the opposite patch, before CNO has looked. Two
# measured examples, both on 4.22.9:
#
#   patch to disable, then enable-frr.sh --wait-only: the daemonset is
#   still 6/6 and co/network is not yet Progressing, so it reported
#   "frr enabled" two seconds into a teardown.
#
#   patch to enable, then disable-frr.sh --wait-only: the namespace does
#   not exist yet and co/network is not yet Progressing, so it reported
#   "frr disabled" while the rollout was starting.
#
# Checking the CR each poll also catches somebody running the opposite
# script halfway through this one, which the normal path could not
# detect either.
frr_wait_guard() {
    local want="$1" intent
    intent="$(frr_intent)"
    if [[ "${intent}" == "unknown" ]]; then
        # Could not find out. Not an answer, so not grounds to abort: an
        # API blip mid-rollout is exactly when this is asked, and
        # aborting on it would fail a run that was going to succeed.
        return 0
    fi
    if [[ "${intent}" != "${want}" ]]; then
        warn "  the Network CR now asks for ${intent}, not ${want}"
        return 2
    fi
    if network_degraded; then
        warn "  co/network went Degraded"
        return 2
    fi
    return 0
}

# Done when the daemonset this patch creates is fully ready, ovnkube-node
# has rolled out with route advertisements on, and CNO has settled. The daemonset is the signal because it is something the patch
# actually changes and the starting state cannot already satisfy in the
# direction that matters -- unlike CRD presence, which a disable only
# partly removes (measured: bgpsessionstates and frrnodestates go,
# frrconfigurations and routeadvertisements stay).
frr_enabled_done() {
    frr_wait_guard enabled || return 2

    local desired ready
    # A read that failed is a failed poll: keep waiting rather than
    # treating it as a daemonset that is not ready.
    desired="$(frr_daemonset desiredNumberScheduled)" || return 1
    ready="$(frr_daemonset numberReady)" || return 1

    if [[ -z "${desired}" || "${desired}" == "0" || "${ready}" != "${desired}" ]]; then
        info "  frr-k8s ${ready:-0}/${desired:-?} ready"
        return 1
    fi
    if ! ovnkube_route_advertisements_rendered; then
        info "  ovnkube-node not yet rendered with route advertisements"
        return 1
    fi
    if ! ovnkube_node_rolled_out; then
        info "  ovnkube-node still rolling out"
        return 1
    fi
    if ! network_settled; then
        info "  co/network still Progressing"
        return 1
    fi
    return 0
}

# Done when CNO has removed the namespace and settled.
frr_disabled_done() {
    frr_wait_guard disabled || return 2

    frr_namespace_present
    case $? in
        0) info "  frr-k8s namespace still present"; return 1 ;;
        2) return 1 ;;
    esac
    if ! network_settled; then
        info "  frr-k8s namespace removed, co/network still Progressing"
        return 1
    fi
    return 0
}
