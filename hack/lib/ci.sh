# shellcheck shell=bash
#
# What the prow jobs need and a developer running the same scripts does
# not, minus anything that knows which cloud this is. Source a cloud's
# own ci.sh rather than this file -- hack/aws/ci.sh, hack/azure/ci.sh --
# because ci_bootstrap is defined there, in terms of what is here plus
# the credentials that cloud needs.
#
# The job is two steps for every cloud, the test and a teardown that
# runs whatever the test did, so both entry points need the same
# bootstrap and it lives here rather than in whichever one was written
# first.

# shellcheck source=hack/lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

ci_workdir=""

# In prow the install leaves a kubeconfig behind in SHARED_DIR. Run
# outside prow and whatever is already in the environment is used
# instead, which is what makes these testable without waiting forty
# minutes for a cluster.
#
# A SHARED_DIR carrying no kubeconfig is a failure rather than a reason
# to fall back: it means the install step did not leave one, and
# carrying on would run against whatever cluster the environment
# happens to point at, which in a CI job is none and on a desk is
# whichever one you last used.
ci_use_shared_kubeconfig() {
    [[ -n "${SHARED_DIR:-}" ]] || return 0
    [[ -f "${SHARED_DIR}/kubeconfig" ]] \
        || die "SHARED_DIR is set but has no kubeconfig" \
               "Looked in ${SHARED_DIR}"
    export KUBECONFIG="${SHARED_DIR}/kubeconfig"
}

# Scratch space only. Nothing that must outlive the run goes in it, and
# nothing created in the cloud is tracked there.
#
# The caller owns the trap that removes it: these scripts have their own
# cleanup to order it against, and a trap set here would be replaced by
# theirs without either of us noticing.
ci_make_workdir() {
    ci_workdir="$(mktemp -d)"
}

# Returns 0 even with nothing to do, because every caller runs this from
# an EXIT trap, where a non-zero status replaces the script's own and
# turns a clean run into a failure.
ci_remove_workdir() {
    [[ -n "${ci_workdir}" ]] && rm -rf "${ci_workdir}"
    return 0
}
