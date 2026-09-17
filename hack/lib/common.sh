# shellcheck shell=bash
#
# Helpers shared by the scripts under hack/. Source this, do not run it.
#
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"
#
# Nothing here knows about any cloud. The per-cloud scripts live under
# hack/aws, hack/azure and hack/gcp and layer their own helpers on top.

# Bash does not propagate errexit into command substitution subshells on
# its own, so `x="$(f)"` runs f with -e switched off and a failure
# halfway through f goes unnoticed -- f returns whatever it had built up
# so far and the caller treats it as an answer. That is how frr_intent
# reported "disabled" for a cluster it could not reach at all. Anything
# sourcing this wants the same rule inside a substitution as outside it.
#
# inherit_errexit arrived in bash 4.4, and macOS still ships 3.2 as
# /bin/bash. There the shopt below fails, sourcing carries on because
# the sourcing script has often not set errexit yet, and every command
# substitution quietly loses the protection just described -- the exact
# failure this line exists to prevent, arriving without a word. Say so
# instead of limping. warn and die are defined further down, so this
# has to speak for itself.
if (( BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 4) )); then
    printf '%s\n' "these scripts need bash 4.4 or newer; this is ${BASH_VERSION}" >&2
    printf '%s\n' "macOS ships 3.2 as /bin/bash -- brew install bash, then run them with that" >&2
    exit 1
fi
shopt -s inherit_errexit

# Every script sourcing this gets the same flag, so --dry-run means the
# same thing everywhere.
dry_run=false

warn() { printf '%s\n' "$*" >&2; }
info() { printf '%s\n' "$*"; }

# Report an action that happened. Silent in a rehearsal, where the
# "would run" line above it is the honest report and an OK underneath
# reads as though the thing was done.
ok() { [[ "${dry_run}" == true ]] || info "OK   $*"; }

# Stop now, for preconditions. Arguments after the first are advice,
# indented one level, line by line so a block quoted back from another
# command keeps its shape.
die() {
    printf '%s\n' "$1" >&2
    shift
    local arg line
    for arg in "$@"; do
        while IFS= read -r line; do printf '  %s\n' "${line}" >&2; done <<<"${arg}"
    done
    exit 1
}

# Parse the flags every script here accepts. Sets a global rather than
# echoing, because a caller would reach for $(parse_args "$@") and a
# subshell cannot set dry_run.
parse_args() {
    local arg
    for arg in "$@"; do
        case "${arg}" in
            --dry-run) dry_run=true ;;
            *) die "unknown option: ${arg}" "Usage: ${0##*/} [--dry-run]" ;;
        esac
    done
}

# Fail once listing everything missing, rather than making the caller
# rerun to discover the next one.
require_cmd() {
    local cmd missing=()
    for cmd in "$@"; do
        command -v "${cmd}" >/dev/null 2>&1 || missing+=("${cmd}")
    done
    (( ${#missing[@]} == 0 )) && return 0
    die "not on PATH: ${missing[*]}"
}

require_cluster() {
    oc whoami >/dev/null 2>&1 || die "not connected to a cluster" \
        "These scripts read the cluster's own idea of where it is running." \
        "export KUBECONFIG=<cluster>/auth/kubeconfig"
}

# Assert the cluster is the cloud the caller was written for, rather
# than letting an Azure job handed an AWS cluster discover it half way
# through building an estate. This is the same field the operator reads.
require_platform() {
    local want="$1" got
    got="$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.type}' 2>&1)" \
        || die "cannot read the cluster platform" "${got}"
    [[ "${got}" == "${want}" ]] && return 0
    die "cluster platform is ${got:-unknown}, this script is for ${want}"
}

# Run a command for its effect, honouring dry_run and returning its
# status rather than exiting, so callers decide what a failure means.
#
# Its stdout is discarded. Every caller here runs something for what it
# does rather than for what it says, and the alternative -- letting call
# sites append >/dev/null -- silently discards the dry-run transcript
# along with the output, so a rehearsal prints nothing and looks like it
# did nothing. Anything whose output you want goes in a command
# substitution instead.
try() {
    if [[ "${dry_run}" == true ]]; then
        info "  would run: $*"
        return 0
    fi
    "$@" >/dev/null
}

# Poll a predicate until it succeeds, or give up and say so. The
# predicate is any command, so callers pass a function name and its
# arguments. Returning non-zero on timeout means the caller has to
# decide, which is the point: the hand-rolled loops this replaces fell
# out silently when they exhausted and the script carried on as though
# the wait had succeeded.
# A predicate returns 0 for done, 1 for not yet, and 2 for "this is
# never going to become true" -- the cluster is heading somewhere else,
# or something failed. Waiting out the deadline on a 2 would turn a
# definite answer into a timeout, so it is passed straight back to the
# caller, which is the only party that knows what it means.
wait_until() {
    local timeout="$1" interval="$2" desc="$3"
    shift 3
    local deadline=$(( SECONDS + timeout )) rc
    while true; do
        # Not `if "$@"; then ...; fi` followed by rc=$?: an if statement
        # whose branch did not run has status 0, so the predicate's own
        # status is lost and an abort reads as "not yet". Assigning
        # through || keeps it, and keeps errexit out of it.
        rc=0
        "$@" || rc=$?
        if (( rc == 0 )); then
            return 0
        fi
        if (( rc == 2 )); then
            return 2
        fi
        if (( SECONDS >= deadline )); then
            warn "  timed out after ${timeout}s waiting for ${desc}"
            return 1
        fi
        sleep "${interval}"
    done
}

# One field per line, empties dropped. `aws --output text` and
# `az -o tsv` both separate with tabs and print a bare newline for an
# empty result, and the obvious `| grep .` filter reports "no match" as
# a failure that then has to be swallowed. Word splitting already drops
# empty fields, so doing it here leaves no status to discard.
print_fields() {
    local field
    for field in $1; do
        printf '%s\n' "${field}"
    done
}

# Retry a command that fails for reasons that are nobody's fault, with
# a fixed delay between attempts. For transient outside-world failures
# only: anything whose failure means the input was wrong will just fail
# N times more slowly.
retry() {
    local attempts="$1" delay="$2" desc="$3"; shift 3
    local n=1
    while true; do
        if "$@"; then
            return 0
        fi
        if (( n >= attempts )); then
            warn "  ${desc}: giving up after ${n} attempt(s)"
            return 1
        fi
        warn "  ${desc}: attempt ${n} of ${attempts} failed, retrying in ${delay}s"
        n=$(( n + 1 ))
        sleep "${delay}"
    done
}

# Teardown needs to record a failure and carry on: stopping at the first
# error is how orphaned resources happen, because everything after it --
# including the summary saying what survived -- never runs.
_failures=()

fail() {
    _failures+=("$1")
    warn "  FAILED: $1"
}

# Final word, and the exit status. Call this last.
report() {
    if (( ${#_failures[@]} == 0 )); then
        if [[ "${dry_run}" == true ]]; then
            info "dry run only, nothing was changed"
        else
            info "done"
        fi
        return 0
    fi
    # A rehearsal that hit failures must say so and exit non-zero, or the
    # rehearsal teaches you the real run would work.
    warn "finished with ${#_failures[@]} failure(s):"
    local f
    for f in "${_failures[@]}"; do warn "  ${f}"; done
    return 1
}
