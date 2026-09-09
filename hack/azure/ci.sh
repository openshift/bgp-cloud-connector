# shellcheck shell=bash
#
# The Azure half of the prow bootstrap. Source this, do not run it: it
# is what the hack/ci-e2e-azure*.sh entry points source instead of
# hack/lib/ci.sh, and it defines ci_bootstrap in terms of the
# cloud-neutral pieces there.
#
# There is no ensure-cli step, unlike AWS. The build root carries no az
# and there is no standalone binary to unzip -- az is a virtualenv --
# so the job's image imports one from ocp:upi-installer at build time
# instead of fetching anything at run time. See the ci-operator config.

# shellcheck source=hack/lib/ci.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/ci.sh"
# shellcheck source=hack/azure/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

# In prow the cluster profile supplies a service principal, in the same
# file and the same four fields openshift-install itself reads. Outside
# prow, whatever `az login` you already have is used, which is what
# makes these runnable against a cluster of your own.
#
# A cluster profile with no service principal in it is a failure rather
# than a reason to fall back: the fallback would reach for a developer's
# own login, and in a job there is none.
ci_azure_credentials() {
    [[ -n "${CLUSTER_PROFILE_DIR:-}" ]] || return 0

    local sp="${CLUSTER_PROFILE_DIR}/osServicePrincipal.json"
    [[ -f "${sp}" ]] \
        || die "CLUSTER_PROFILE_DIR is set but has no osServicePrincipal.json" \
               "Looked in ${CLUSTER_PROFILE_DIR}"

    require_cmd az jq

    # az keeps its token cache, its profile and the subscription it is
    # pointed at under AZURE_CONFIG_DIR, defaulting to $HOME/.azure. A
    # prow test container runs as a random uid whose home it may not own,
    # so it is told where to put them. The scratch directory is the right
    # place: it goes away with the run, and takes the service principal's
    # token cache with it.
    export AZURE_CONFIG_DIR="${ci_workdir}/azure"
    mkdir -p "${AZURE_CONFIG_DIR}"

    # -e as well as -r: without it jq prints the string "null" and exits
    # 0 for a key that is not there, so a service principal missing a
    # field would reach az as --tenant null and be reported as a failed
    # login rather than as a malformed file.
    local client_id tenant_id subscription_id client_secret
    client_id="$(jq -er .clientId "${sp}")" \
        || die "no clientId in ${sp}"
    tenant_id="$(jq -er .tenantId "${sp}")" \
        || die "no tenantId in ${sp}"
    subscription_id="$(jq -er .subscriptionId "${sp}")" \
        || die "no subscriptionId in ${sp}"
    client_secret="$(jq -er .clientSecret "${sp}")" \
        || die "no clientSecret in ${sp}"

    # The secret is read straight out of the file into the argument and
    # never into a variable this function prints, because prow logs for
    # openshift repositories are public. --output none for the same
    # reason: a successful login otherwise prints the subscription, the
    # tenant and the signed-in principal.
    # Nothing here prints the secret, and --output none is for the same
    # reason: prow logs for openshift repositories are public, and a
    # successful login otherwise prints the subscription, the tenant and
    # the signed-in principal.
    az login --service-principal \
        --username "${client_id}" \
        --password "${client_secret}" \
        --tenant "${tenant_id}" \
        --output none \
        || die "could not log in as the service principal in ${sp}"

    az account set --subscription "${subscription_id}" \
        || die "could not select the subscription named in ${sp}"
}

ci_bootstrap() {
    ci_use_shared_kubeconfig
    # Before the credentials, unlike the AWS bootstrap, because the
    # login needs somewhere to put its token cache and the scratch
    # directory is where it goes.
    ci_make_workdir
    ci_azure_credentials
}
