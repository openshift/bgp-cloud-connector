#!/usr/bin/env bash
#
# Entry point for the e2e-gcp prow job: run the test, capture what it
# said, then tear down whatever is there. Always.
#
#   hack/ci-e2e-gcp.sh
#
# The sequencing is ours rather than ci-operator's for two reasons.
#
# A test that specifies post steps overrides the workflow's post rather
# than adding to it -- see mergeWorkflow in ci-tools' registry resolver
# -- so a teardown expressed that way would replace ipi-gcp-post and
# take the cluster deprovision with it. Leaking an NCC hub is the
# problem we are solving; leaking the whole cluster would be worse.
#
# And the order has to be ours anyway. The hub refuses to go while the
# operator's spoke is attached, so the CRs have to be removed before the
# estate, and the estate before the cluster is deprovisioned.
#
# The two halves know nothing about each other. ci-e2e-gcp-run.sh
# creates and never removes, ci-e2e-gcp-teardown.sh removes and never
# creates, and this file is the only place that says "always".

set -o nounset
set -o pipefail
# Deliberately no errexit: running the teardown after a failed test is
# the entire job of this file, and errexit would exit before it.

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/gcp/ci.sh
source "${here}/gcp/ci.sh"

teardown_done=false

# Read the cluster's identity once, here, before anything is created,
# and hand it to the teardown. Without it the teardown asks the cluster
# who it is, and a cluster that has stopped answering by then takes the
# cloud cleanup down with it -- which is the one failure this file
# exists to prevent. Failing here instead costs nothing: nothing has
# been built yet.
ci_bootstrap
trap ci_remove_workdir EXIT

require_cmd gcloud oc
require_cluster
require_platform GCP
gcp_cluster_facts

# The teardown is idempotent and treats "nothing there" as success, so
# running it after a test that failed before creating anything costs a
# couple of API calls and reports done. That is what lets this be
# unconditional rather than conditional on how far the test got.
run_teardown() {
    if [[ "${teardown_done}" == true ]]; then
        return 0
    fi
    teardown_done=true
    info "--- teardown ---"
    INFRA="${infra}" GCP_PROJECT="${project}" GCP_REGION="${region}" \
        "${here}/ci-e2e-gcp-teardown.sh"
}

# Prow signals rather than exits, and bash runs no EXIT trap when an
# untrapped signal kills the shell. A trap is not a guarantee -- once
# the grace period is up the next signal is KILL and nothing runs -- but
# it converts the ordinary cancellation into a clean teardown, and an
# NCC hub left behind blocks nothing but bills and confuses the next run.
test_pid=0

# Invoked from the traps below.
# shellcheck disable=SC2329
on_signal() {
    warn "--- caught SIG$1, tearing down before exiting ---"
    # The whole test group, not just the pid we started.
    # ci-e2e-gcp-run.sh is a sequence of other scripts, and killing only
    # it orphans whichever one is running rather than stopping it: the
    # teardown then deletes an estate that a create it cannot see is
    # still adding to. The test is started in its own process group so
    # this signal reaches all of it and none of us.
    if (( test_pid > 0 )); then
        kill -TERM -"${test_pid}" 2>/dev/null || true
        wait "${test_pid}" 2>/dev/null || true
    fi
    if ! run_teardown; then
        warn "--- teardown reported a failure; see its output above ---"
    fi
    exit "$2"
}
trap 'on_signal TERM 143' TERM
trap 'on_signal INT 130' INT

info "--- test ---"
test_rc=0
# Backgrounded, not because anything runs concurrently, but because bash
# defers a trap until the foreground command finishes. Signalled at this
# pid alone, a foreground test runs to completion first and the whole
# grace period is spent before the teardown begins. Waiting on a
# background child is interruptible, so the trap fires when the signal
# arrives and hands the remaining time to the teardown.
#
# In its own process group, so on_signal can stop the test and
# everything it spawned without stopping this script, which still has
# the teardown to run.
#
# Job control rather than setsid, which hack/ci-e2e-aws.sh still uses.
# setsid forks when its caller is already a process group leader, and
# then $! is the pid of a parent that exits at once: wait returns
# immediately while the test runs on, and kill -TERM -$! answers "no
# such process group". It takes monitor mode to reach, which neither
# prow nor a shell prompt turns on for a script, so it is a sharp edge
# rather than a live bug -- but job control costs nothing and cannot
# fail that way.
set -m
"${here}/ci-e2e-gcp-run.sh" &
test_pid=$!
set +m
wait "${test_pid}" || test_rc=$?
test_pid=0
if (( test_rc == 0 )); then
    info "OK   test passed"
else
    warn "test FAILED, exit ${test_rc}"
fi

teardown_rc=0
run_teardown || teardown_rc=$?
if (( teardown_rc != 0 )); then
    warn "--- teardown FAILED: cloud resources may still be up ---"
fi

# The test's verdict wins, because that is what the job is reporting on.
# A teardown failure only decides the outcome when there was nothing
# else wrong -- but it does decide it, because resources left running
# are not a pass.
if (( test_rc != 0 )); then
    exit "${test_rc}"
fi
exit "${teardown_rc}"
