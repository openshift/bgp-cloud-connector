# shellcheck shell=bash
# Shared by hack/ci-e2e-aws.sh and its test. Sourced, never run.

# AWS reports "not ready yet" as IncorrectState rather than as a
# retryable error code, and the describe APIs answer optimistically: a
# resource still deleting is already absent from the "what is left"
# queries. Polling those for "has it gone" returned true on the first
# attempt in CI, and the disassociate that followed was rejected.
#
# So do not predict readiness. Issue the call and let AWS say when it
# is ready. This also means the teardown does not have to know the
# dependency order exactly -- the order below only helps it converge
# sooner.
retry_on_incorrect_state() {
    local what="$1" budget="$2"; shift 2
    local deadline=$((SECONDS + budget)) out
    local interval="${RETRY_INTERVAL_SECS:-10}"

    while true; do
        if out="$("$@" 2>&1)"; then
            printf '%s\n' "${out}"
            return 0
        fi
        if [[ "${out}" != *IncorrectState* ]]; then
            echo "${what}: ${out}" >&2
            return 1
        fi
        if (( SECONDS >= deadline )); then
            echo "${what}: still IncorrectState after ${budget}s: ${out}" >&2
            return 1
        fi
        sleep "${interval}"
    done
}

# Azure refuses a mutation that collides with one already in flight,
# with AnotherOperationInProgress, and the refusal is not a failure so
# much as a "not yet": it clears when the operation in flight finishes.
#
# Sized in minutes rather than seconds because of what that operation
# usually is. A run cancelled part way through a Route Server create
# leaves the create running server-side, and Azure will not delete the
# Route Server until it completes -- measured at about fifteen minutes.
# The teardown that follows a cancellation is exactly the case this
# matters for, and giving up early there leaks a Route Server, its
# public IP, the subnet holding it, and the address prefix, all of
# which then block the cluster's own deprovision.
#
# Anything else is passed straight back. An authorization failure or a
# resource that does not exist will not clear by waiting, and retrying
# it only fails more slowly.
retry_on_azure_conflict() {
    local what="$1" budget="$2"; shift 2
    local deadline=$((SECONDS + budget)) out
    local interval="${RETRY_INTERVAL_SECS:-15}"

    while true; do
        if out="$("$@" 2>&1)"; then
            printf '%s\n' "${out}"
            return 0
        fi
        # Two codes, both observed on 2026-09-09 deleting the same
        # Route Server, and both meaning "not now" rather than "no".
        #
        # AnotherOperationInProgress is a create still running
        # server-side after a cancelled run, which is the case this
        # function exists for. InternalServerError arrived once that
        # cleared, on the very next attempt, and the delete then
        # succeeded on a later one: Azure returns 500s on this API under
        # load, and treating one as final leaks the whole estate.
        #
        # Everything else is passed straight back. An authorization
        # failure or a resource that is genuinely in use will not clear
        # by waiting, and retrying only fails more slowly.
        if [[ "${out}" != *AnotherOperationInProgress* \
           && "${out}" != *InternalServerError* ]]; then
            echo "${what}: ${out}" >&2
            return 1
        fi
        if (( SECONDS >= deadline )); then
            echo "${what}: still refused after ${budget}s: ${out}" >&2
            return 1
        fi
        sleep "${interval}"
    done
}
