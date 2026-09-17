# shellcheck shell=bash
#
# The GCP half of the prow bootstrap. Source this, do not run it: it is
# what the hack/ci-e2e-gcp*.sh entry points source instead of
# hack/lib/ci.sh, and it defines ci_bootstrap in terms of the
# cloud-neutral pieces there.
#
# Everything that knows the credentials are Google's is in this file, so
# that adding a cloud adds a file beside it rather than a branch inside
# lib/ci.sh.

# shellcheck source=hack/lib/ci.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/ci.sh"
# shellcheck source=hack/gcp/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

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
    # openshift-install and the Go SDK read this one; gcloud reads the
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
#
# Installing into the repository's bin rather than a scratch directory
# is what keeps the sequencer's two children sharing a single download
# instead of fetching eighty megabytes each.
ci_ensure_gcloud() {
    local dir
    dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/ensure-cli.sh"
    dir="$("${dir}")" || die "could not provide a gcloud"
    export PATH="${dir}:${PATH}"
}

ci_bootstrap() {
    ci_use_shared_kubeconfig
    ci_make_workdir
    # After ci_make_workdir, which is what defines ci_workdir.
    ci_gcp_credentials
    ci_ensure_gcloud
}
