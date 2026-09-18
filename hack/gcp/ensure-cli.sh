#!/usr/bin/env bash
#
# Make sure a gcloud new enough for the estate commands is available,
# and print the directory it is in.
#
#   hack/gcp/ensure-cli.sh [<install-dir>]
#
# Install dir defaults to the repository's bin, which .gitignore already
# covers.
#
# Nothing is downloaded when the one on PATH will do. That is the rule
# hack/aws/ensure-cli.sh uses and it matters more here: the archive is
# eighty-three megabytes.
#
# This is why GCP needs no image of its own, unlike Azure. az is a
# Python distribution with nothing to fetch, so e2e-azure-operator has
# to run in an image with it baked in; the Google Cloud CLI ships as a
# self-contained tarball carrying its own interpreter, exactly like the
# aws CLI's zip, so e2e-gcp-operator can stay on `from: src`.
#
# The check is on the version rather than on presence, for the reason
# the aws one gives: something old enough to satisfy "is it installed?"
# then fails on a subcommand, a long way from the cause.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

# network-connectivity reached GA in 400-ish; this is comfortably past
# it and past the spoke flags the estate uses.
min_version="450.0.0"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
install_dir="${1:-${repo_root}/bin}"

case "$(uname -m)" in
    x86_64|amd64)  arch="x86_64" ;;
    aarch64|arm64) arch="arm" ;;
    *) die "no Google Cloud CLI archive is published for $(uname -m)" ;;
esac

# gcloud version prints several lines; the first is
# "Google Cloud SDK 542.0.0".
gcloud_version() {
    local out
    out="$("$1" version 2>/dev/null)" || return 1
    printf '%s' "${out}" | sed -n 's|^Google Cloud SDK \([0-9.]*\).*|\1|p' | head -1
}

version_at_least() {
    [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" == "$2" ]]
}

usable() {
    local candidate="$1" version
    version="$(gcloud_version "${candidate}")" || return 1
    [[ -n "${version}" ]] || return 1
    version_at_least "${version}" "${min_version}"
}

if command -v gcloud >/dev/null 2>&1 && usable gcloud; then
    dirname "$(command -v gcloud)"
    exit 0
fi

if [[ -x "${install_dir}/gcloud" ]] && usable "${install_dir}/gcloud"; then
    printf '%s' "${install_dir}"
    exit 0
fi

require_cmd curl tar

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

# Fetched and checked in one step, so a retry covers a truncated
# download as well as a failed one: a corrupt archive otherwise fails in
# the unpack, where it reads as a broken release rather than a bad
# transfer.
fetch() {
    local tgz="$1"
    rm -f "${tgz}"
    curl -fsSL --retry 3 --retry-delay 2 \
        "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-linux-${arch}.tar.gz" \
        -o "${tgz}" || return 1
    tar -tzf "${tgz}" >/dev/null 2>&1 || return 1
    return 0
}

tgz="${workdir}/google-cloud-cli.tar.gz"

# Retried because in CI this runs after the cluster is up. Throwing away
# an hour's install because a CDN blipped once is the most expensive way
# to fail, and it is the first outside thing the job touches.
warn "no gcloud ${min_version} or newer on PATH; fetching ${arch} into ${install_dir}"
retry 5 10 "fetch the Google Cloud CLI" fetch "${tgz}" \
    || die "could not download a usable Google Cloud CLI archive"

tar -xzf "${tgz}" -C "${workdir}"
[[ -x "${workdir}/google-cloud-sdk/bin/gcloud" ]] \
    || die "the archive unpacked without a gcloud in it"

mkdir -p "${install_dir}"
# The SDK is not relocatable by copying the binary alone: it is a tree
# with its own Python and libraries beside it, so the tree moves and the
# entry point is a symlink into it.
rm -rf "${install_dir}/google-cloud-sdk"
mv "${workdir}/google-cloud-sdk" "${install_dir}/google-cloud-sdk"
ln -sf "google-cloud-sdk/bin/gcloud" "${install_dir}/gcloud"

usable "${install_dir}/gcloud" \
    || die "the gcloud that was installed does not report ${min_version} or newer"

printf '%s' "${install_dir}"
