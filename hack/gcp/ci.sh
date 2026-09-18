# shellcheck shell=bash
#
# What the GCP prow job needs and a developer running the same script
# does not. Source this, do not run it.
#
# Deliberately standalone rather than built on hack/lib/ci.sh. That file
# is the AWS bootstrap -- it sources hack/aws/lib.sh and its ci_bootstrap
# installs the aws CLI -- so reusing it would mean refactoring the AWS
# path to add a GCP preflight, which is a change to a job this one has
# no business touching. The kubeconfig and workdir handling below is the
# same shape as its, and the two should be folded together when the rest
# of the GCP e2e job lands and the split is worth doing.

# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

ci_workdir=""

# In prow the cluster profile supplies the key. Outside it, whatever the
# process already has is used -- an ordinary gcloud login, or a
# GOOGLE_APPLICATION_CREDENTIALS somebody exported.
#
# A cluster profile with no key in it is a failure rather than a reason
# to fall back: the fallback would reach for a developer's own login,
# and in a job there is none.
#
# CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE rather than
# `gcloud auth activate-service-account`, because the override needs no
# writable gcloud config and mutates nothing outside this process. That
# also makes it the answer to "how do I log in headlessly": there is no
# headless `gcloud auth login`, and --no-launch-browser still wants a
# browser somewhere and a code pasted back.
ci_gcp_credentials() {
    [[ -n "${CLUSTER_PROFILE_DIR:-}" ]] || return 0

    local key="${CLUSTER_PROFILE_DIR}/gce.json"
    [[ -f "${key}" ]] \
        || die "CLUSTER_PROFILE_DIR is set but has no gce.json" \
               "Looked in ${CLUSTER_PROFILE_DIR}"

    export CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="${key}"
    # The Go SDK and openshift-install read this one; gcloud reads the
    # override above. Both are set so the whole run uses one identity.
    export GOOGLE_APPLICATION_CREDENTIALS="${key}"

    # gcloud writes its config and logs under CLOUDSDK_CONFIG,
    # defaulting to $HOME/.config/gcloud. A prow test container runs as
    # a random uid whose home it may not own, so it is told where to put
    # them: the scratch directory, which goes away with the run.
    export CLOUDSDK_CONFIG="${ci_workdir}/gcloud"
    mkdir -p "${CLOUDSDK_CONFIG}"

    # Nothing here prints the key or the account. Prow logs for
    # openshift repositories are public.
    export CLOUDSDK_CORE_DISABLE_PROMPTS=1
}

# Put a usable gcloud on PATH. hack/gcp/ensure-cli.sh decides whether
# that means the one already installed or a download, so there is one
# implementation of the decision.
ci_ensure_gcloud() {
    local dir
    dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ensure-cli.sh"
    dir="$("${dir}")" || die "could not provide a gcloud"
    export PATH="${dir}:${PATH}"
}

# The caller owns the trap that removes the workdir, so that a script
# with its own cleanup can order the two.
ci_gcp_bootstrap() {
    if [[ -n "${SHARED_DIR:-}" ]]; then
        [[ -f "${SHARED_DIR}/kubeconfig" ]] \
            || die "SHARED_DIR is set but has no kubeconfig" \
                   "Looked in ${SHARED_DIR}"
        export KUBECONFIG="${SHARED_DIR}/kubeconfig"
    fi

    ci_workdir="$(mktemp -d)"
    ci_gcp_credentials
    ci_ensure_gcloud
}

ci_remove_gcp_workdir() {
    [[ -n "${ci_workdir}" ]] && rm -rf "${ci_workdir}"
    return 0
}
