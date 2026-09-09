#!/usr/bin/env bash
#
# Unit tests for the shell libraries under hack/lib.
#
#   hack/lib-test.sh
#
# Everything here runs without a cluster and without AWS. The cluster
# functions reach the outside world only through oc, so overriding that
# one function is the entire fixture -- which is the point: the two
# false-success bugs in the FRR predicates were found by a six-minute
# round trip against a real cluster and are reproduced below in
# milliseconds.

set -o nounset
set -o pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/lib/frr.sh
source "${here}/lib/frr.sh"       # pulls in common.sh
# shellcheck source=hack/lib/retry.sh
source "${here}/lib/retry.sh"
# shellcheck source=hack/aws/lib.sh
source "${here}/aws/lib.sh"

export RETRY_INTERVAL_SECS=1      # real sleeps would be too slow for the unit job

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

passed=0
failed=0

check() {
    if [[ "$2" == "$3" ]]; then
        echo "PASS $1"
        passed=$((passed + 1))
    else
        echo "FAIL $1: got '$2', want '$3'"
        failed=$((failed + 1))
    fi
}

######################################################################
echo "--- lib/common.sh ---"

dry_run=true
check "try reports rather than runs in a rehearsal" \
    "$(try echo should-not-run)" "  would run: echo should-not-run"
check "ok is silent in a rehearsal" "$(ok "did a thing")" ""
dry_run=false
try touch "${workdir}/try-ran"
check "try runs for real otherwise" \
    "$([[ -f "${workdir}/try-ran" ]] && echo ran)" "ran"
check "try discards what the command says" "$(try echo noisy)" ""
check "ok reports when not rehearsing" "$(ok "did a thing")" "OK   did a thing"

check "parse_args accepts --dry-run" \
    "$(dry_run=false; parse_args --dry-run; echo "${dry_run}")" "true"
(parse_args --nonsense) >/dev/null 2>&1
check "parse_args rejects anything else" "$?" "1"

# wait_until's contract: 0 done, 1 timed out, 2 never going to happen.
never() { return 1; }
now()   { return 0; }
nope()  { return 2; }

check "wait_until returns 0 when the predicate is satisfied" \
    "$(wait_until 5 1 desc now; echo $?)" "0"

started=${SECONDS}
wait_until 2 1 desc never >/dev/null 2>&1
rc=$?; elapsed=$((SECONDS - started))
check "wait_until times out with 1" "${rc}" "1"
check "wait_until waited for its budget" "$(( elapsed >= 2 ))" "1"

# The abort code must come back at once. Sitting on it until the
# deadline would turn a definite answer into a timeout, and would have
# masked the FRR bugs below as slowness rather than wrongness.
started=${SECONDS}
wait_until 30 1 desc nope >/dev/null 2>&1
rc=$?; elapsed=$((SECONDS - started))
check "wait_until passes an abort straight back" "${rc}" "2"
check "wait_until does not wait out the deadline on an abort" "$(( elapsed <= 2 ))" "1"

# retry is for transient outside-world failures. The aws CLI download
# runs after the cluster is up, so a single CDN blip there throws away a
# forty-minute install.
flaky_calls=0
flaky() { flaky_calls=$((flaky_calls + 1)); (( flaky_calls >= 3 )); }
always_fails() { return 1; }
first_time() { return 0; }

started=${SECONDS}
retry 5 1 "flaky" first_time >/dev/null 2>&1
check "retry succeeds first time" "$?" "0"
check "retry does not sleep when it succeeds first time" "$(( SECONDS - started <= 1 ))" "1"

flaky_calls=0
retry 5 1 "flaky" flaky >/dev/null 2>&1
check "retry keeps going until it succeeds" "$?" "0"
check "retry stopped as soon as it succeeded" "${flaky_calls}" "3"

retry 3 1 "doomed" always_fails >/dev/null 2>&1
check "retry gives up after the last attempt" "$?" "1"

# print_fields is shared rather than AWS's, because `az -o tsv` has the
# same shape as `aws --output text`: tab separated, and a bare newline
# for an empty result.
check "print_fields puts one field per line" \
    "$(print_fields "$(printf 'a\tb\tc')" | tr '\n' ' ')" "a b c "
check "print_fields says nothing for an empty result" \
    "$(print_fields "$(printf '\n')" | wc -l)" "0"
check "print_fields drops empty fields rather than emitting blank lines" \
    "$(print_fields "$(printf 'a\t\tb')" | wc -l)" "2"
check "print_fields handles a newline separated result too" \
    "$(print_fields "$(printf 'a\nb')" | tr '\n' ' ')" "a b "

# The point of it living here rather than in aws/lib.sh: a cloud that
# does not source AWS's library still gets it.
check "print_fields comes from common.sh alone" \
    "$(bash -c 'source "${0}/lib/common.sh"; print_fields "$(printf "a\tb")" | tr "\n" " "' "${here}")" \
    "a b "


######################################################################
echo "--- lib/frr.sh ---"

# The whole cluster, as far as these functions can tell.
stub_caps=""; stub_ra=""; stub_degraded="False"
stub_progressing="False"; stub_ns="absent"; stub_desired=""; stub_ready=""

oc() {
    case "$*" in
        *additionalRoutingCapabilities*) printf '%s' "${stub_caps}" ;;
        *routeAdvertisements*)           printf '%s' "${stub_ra}" ;;
        *Degraded*)                      printf '%s' "${stub_degraded}" ;;
        *Progressing*)                   printf '%s' "${stub_progressing}" ;;
        *"ns openshift-frr-k8s"*)
            case "${stub_ns}" in
                present) return 0 ;;
                absent)  echo 'Error from server (NotFound): namespaces "openshift-frr-k8s" not found' >&2; return 1 ;;
                *)       echo "The connection to the server was refused" >&2; return 1 ;;
            esac ;;
        *desiredNumberScheduled*)        printf '%s' "${stub_desired}" ;;
        *numberReady*)                   printf '%s' "${stub_ready}" ;;
        *) echo "unstubbed oc call: $*" >&2; return 1 ;;
    esac
}

enabled_cluster() {
    stub_caps='{"providers":["FRR"]}'; stub_ra="Enabled"
    stub_ns="present"; stub_desired="6"; stub_ready="6"
    stub_progressing="False"; stub_degraded="False"
}
disabled_cluster() {
    stub_caps=""; stub_ra="Disabled"
    stub_ns="absent"; stub_desired=""; stub_ready=""
    stub_progressing="False"; stub_degraded="False"
}

enabled_cluster
check "frr_intent reads enabled" "$(frr_intent)" "enabled"
disabled_cluster
check "frr_intent reads disabled" "$(frr_intent)" "disabled"
stub_caps=""; stub_ra="Enabled"
check "frr_intent calls a half-applied CR mixed" "$(frr_intent)" "mixed"
stub_caps='{"providers":["FRR"]}'; stub_ra="Disabled"
check "frr_intent calls the other half mixed too" "$(frr_intent)" "mixed"

enabled_cluster
check "frr_enabled_done is done on an enabled, settled cluster" \
    "$(frr_enabled_done >/dev/null; echo $?)" "0"
stub_ready="3"
check "frr_enabled_done waits while the daemonset is partial" \
    "$(frr_enabled_done >/dev/null; echo $?)" "1"
enabled_cluster; stub_progressing="True"
check "frr_enabled_done waits while co/network is Progressing" \
    "$(frr_enabled_done >/dev/null; echo $?)" "1"

# Regression, measured on 4.22.9: patch to disable and the daemonset is
# still 6/6 while co/network has not gone Progressing yet, because CNO
# has not looked. enable-frr.sh --wait-only reported "frr enabled" two
# seconds into a teardown.
enabled_cluster; stub_caps=""; stub_ra="Disabled"
check "frr_enabled_done aborts when the CR asks for disabled" \
    "$(frr_enabled_done >/dev/null 2>&1; echo $?)" "2"

disabled_cluster
check "frr_disabled_done is done on a disabled, settled cluster" \
    "$(frr_disabled_done >/dev/null; echo $?)" "0"
disabled_cluster; stub_ns="present"
check "frr_disabled_done waits while the namespace remains" \
    "$(frr_disabled_done >/dev/null; echo $?)" "1"
disabled_cluster; stub_progressing="True"
check "frr_disabled_done waits while co/network is Progressing" \
    "$(frr_disabled_done >/dev/null; echo $?)" "1"

# A namespace we could not read is not a namespace that has gone. The
# guard above catches a cluster that has stopped answering altogether,
# because the intent read fails too; this is the narrower case where the
# Network CR is readable and the namespace is not, which finishes the
# wait on the one answer it must never assume.
disabled_cluster; stub_ns="unreadable"
check "frr_disabled_done keeps waiting when the namespace read fails" \
    "$(frr_disabled_done >/dev/null 2>&1; echo $?)" "1"

# The mirror image: patch to enable and the namespace does not exist
# yet, so disable-frr.sh --wait-only reported "frr disabled" while the
# rollout was starting.
disabled_cluster; stub_caps='{"providers":["FRR"]}'; stub_ra="Enabled"
check "frr_disabled_done aborts when the CR asks for enabled" \
    "$(frr_disabled_done >/dev/null 2>&1; echo $?)" "2"

# A cluster we cannot reach must not read as a cluster that is
# deliberately disabled. Both fields come back empty either way, so
# without a distinct answer an unreachable API looks exactly like a
# deliberate disable -- and every caller acts on it.
oc_broken() { echo "The connection to the server was refused" >&2; return 1; }

enabled_cluster
check "frr_intent reports unknown when oc fails" \
    "$(oc() { oc_broken; }; frr_intent 2>/dev/null)" "unknown"

# And a failed read mid-poll is a failed poll, not grounds to abort a
# rollout that was going to succeed.
check "frr_enabled_done keeps waiting when oc fails" \
    "$(oc() { oc_broken; }; frr_enabled_done >/dev/null 2>&1; echo $?)" "1"
check "frr_disabled_done keeps waiting when oc fails" \
    "$(oc() { oc_broken; }; frr_disabled_done >/dev/null 2>&1; echo $?)" "1"

enabled_cluster; stub_degraded="True"
check "frr_enabled_done aborts when co/network is Degraded" \
    "$(frr_enabled_done >/dev/null 2>&1; echo $?)" "2"
disabled_cluster; stub_degraded="True"
check "frr_disabled_done aborts when co/network is Degraded" \
    "$(frr_disabled_done >/dev/null 2>&1; echo $?)" "2"

unset -f oc

######################################################################
echo "--- aws/lib.sh ---"
#
# Same rule as the FRR intent, and for the same reason: these decide
# whether to create something or whether there is anything to tear down,
# so a failed describe read as "nothing there" either builds a second
# estate alongside the first or reports a teardown complete while the
# first survives. Expired credentials mid-run are the ordinary way it
# happens and they are silent.

# Invoked indirectly, through aws_query's "$@".
# shellcheck disable=SC2329
aws_ok()     { printf '%s' "${stub_aws_out}"; }
aws_broken() { echo "An error occurred (ExpiredToken) when calling the DescribeRouteServers operation" >&2; return 255; }

stub_aws_out=""

# shellcheck disable=SC2329
aws() { aws_ok; }
stub_aws_out="rs-0123456789abcdef0"
check "route_server_for_cluster returns the id" \
    "$(route_server_for_cluster mycluster)" "rs-0123456789abcdef0"

stub_aws_out="None"
check "route_server_for_cluster returns empty when there is none" \
    "$(route_server_for_cluster mycluster)" ""

# The teardown prints "nothing to do" and exits 0 on an empty answer, so
# this must not be reachable by a failed call.
# shellcheck disable=SC2329
aws() { aws_broken; }
(route_server_for_cluster mycluster) >/dev/null 2>&1
check "route_server_for_cluster fails rather than reporting none" "$?" "1"
# And it must fail without relying on the caller having set errexit.
check "route_server_for_cluster fails without errexit in the caller" \
    "$(set +e; route_server_for_cluster mycluster >/dev/null 2>&1; echo $?)" "1"
check "route_server_for_cluster says why" \
    "$( (route_server_for_cluster mycluster) 2>&1 >/dev/null | head -1)" \
    "cannot list route servers tagged mycluster-rs"

# A failed count reads as zero, and zero means "create the full set
# again" -- a duplicate, billable estate beside the one already there.
(live_endpoints_in_subnet rs-1 subnet-1) >/dev/null 2>&1
check "live_endpoints_in_subnet fails rather than reporting zero" "$?" "1"

# shellcheck disable=SC2329
aws() { aws_ok; }
stub_aws_out="2"
check "live_endpoints_in_subnet returns the count" \
    "$(live_endpoints_in_subnet rs-1 subnet-1)" "2"

# get-route-server-associations reports "none" as an error, so that one
# error string genuinely means empty and the rest do not.
aws() { echo "An error occurred (InvalidRouteServerId.NotAssociated) when calling the GetRouteServerAssociations operation" >&2; return 255; }
check "route_server_vpcs treats NotAssociated as none" "$(route_server_vpcs rs-1)" ""
(route_server_vpcs rs-1) >/dev/null 2>&1
check "route_server_vpcs succeeds on NotAssociated" "$?" "0"

# shellcheck disable=SC2329
aws() { aws_broken; }
(route_server_vpcs rs-1) >/dev/null 2>&1
check "route_server_vpcs fails on any other error" "$?" "1"

# shellcheck disable=SC2329
aws() { aws_ok; }
stub_aws_out="vpc-aaa"
check "route_server_vpcs returns the vpcs" "$(route_server_vpcs rs-1)" "vpc-aaa"

# The CLI writes to stderr on calls it goes on to answer -- a deprecation
# notice, a retry -- and folding that in makes the warning part of the
# id, which then goes back to AWS as --vpc-id.
# shellcheck disable=SC2329
aws() { echo "UserWarning: urllib3 is compiled with an old OpenSSL" >&2; aws_ok; }
stub_aws_out="vpc-aaa"
check "route_server_vpcs keeps stderr out of the vpcs" \
    "$(route_server_vpcs rs-1 2>/dev/null)" "vpc-aaa"

# What the teardown lists is what the teardown deletes, so a state left
# out of these two is a resource nothing ever tries to remove. "failed"
# is the one that matters: it is not available, so it went unlisted, and
# the route server then refuses to go while it is still there and the
# endpoint keeps an ENI in the subnet the installer is about to delete.
# Every other helper here selects on "not deleted and not deleting";
# these two now do too. The stub hands back the query so the states can
# be read off it -- there is nothing else to observe without an EC2.
# shellcheck disable=SC2329
aws() {
    local arg
    for arg in "$@"; do
        case "${arg}" in RouteServer*) printf '%s' "${arg}" ;; esac
    done
}
check "route_server_endpoints does not list only the available ones" \
    "$(route_server_endpoints rs-1 | grep -c "State=='available'" || true)" "0"
check "route_server_endpoints skips the dead states and nothing else" \
    "$(route_server_endpoints rs-1 | grep -c "State!='deleted' && State!='deleting'" || true)" "1"
check "endpoint_peers does not list only the available ones" \
    "$(endpoint_peers rsep-1 | grep -c "State=='available'" || true)" "0"
check "endpoint_peers skips the dead states and nothing else" \
    "$(endpoint_peers rsep-1 | grep -c "State!='deleted' && State!='deleting'" || true)" "1"

unset -f aws

######################################################################
echo "--- lib/retry.sh ---"
#
# The teardown has two claims behind it. That AWS answers "not ready
# yet" with IncorrectState is established by observation -- a prow run
# on 2026-08-24 was rejected with "Route Server has non-deleted
# propagation to Route Table" and leaked a route server. That the retry
# handles it is what these check, with a stub rather than AWS, so they
# do not depend on a live run happening to hit the race. An earlier fix
# passed against a real cluster and still leaked in CI precisely because
# a green run proves nothing when the bug is a race.

# The retry runs its command in a command substitution, so a counter
# kept in a variable would be incremented in a subshell and lost.
attempts() { cat "${workdir}/$1" 2>/dev/null || echo 0; }
record() {
    local n
    n=$(( $(attempts "$1") + 1 ))
    echo "${n}" > "${workdir}/$1"
    echo "${n}"
}

incorrect_state() {
    echo "An error occurred (IncorrectState) when calling the DisassociateRouteServer operation" >&2
    return 255
}

# Rejected twice, then accepted: the shape of a disassociate waiting on
# a propagation to finish deleting.
settles() {
    local n
    n="$(record settles)"
    if [[ "${n}" -lt 3 ]]; then
        incorrect_state
        return
    fi
    echo "disassociated"
}

out="$(retry_on_incorrect_state "settles" 60 settles 2>&1)"
check "retried until accepted" "$?" "0"
check "retried the right number of times" "$(attempts settles)" "3"
check "output of the accepted call is returned" "${out}" "disassociated"

# A permissions problem must surface at once. Retrying it for ten
# minutes and then reporting a timeout would hide the actual cause.
denied() {
    record denied >/dev/null
    echo "An error occurred (UnauthorizedOperation) when calling the DeleteRouteServer operation" >&2
    return 255
}

retry_on_incorrect_state "denied" 60 denied >/dev/null 2>&1
check "a non-IncorrectState error fails immediately" "$?" "1"
check "a non-IncorrectState error is not retried" "$(attempts denied)" "1"

# Something genuinely stuck has to give up rather than run until the
# step's grace period kills it mid-teardown.
stuck() {
    record stuck >/dev/null
    incorrect_state
}

started=${SECONDS}
retry_on_incorrect_state "stuck" 3 stuck >/dev/null 2>&1
rc=$?
elapsed=$((SECONDS - started))
check "a permanent IncorrectState gives up" "${rc}" "1"
check "gave up near its budget" "$(( elapsed >= 3 && elapsed <= 15 ))" "1"

# The overwhelmingly common case, and it must not sleep at all.
accepted() { echo "fine"; }

started=${SECONDS}
out="$(retry_on_incorrect_state "accepted" 60 accepted 2>&1)"
rc=$?
elapsed=$((SECONDS - started))
check "a call accepted first time succeeds" "${rc}" "0"
check "a call accepted first time returns its output" "${out}" "fine"
check "a call accepted first time did not sleep" "$(( elapsed <= 2 ))" "1"

######################################################################
echo "--- aws/lib.sh: aws_cluster_vpc ---"

# The VPC has to be found on a cluster whose VPC the installer did not
# create. ROSA HCP takes a BYO VPC, so nothing tags it
# kubernetes.io/cluster/<infra>=owned, and a lookup that depends on that
# tag finds nothing. A node is in the cluster's VPC whoever built it, so
# that is what these fixtures describe.

stub_provider_id=""
stub_instance_vpc=""
stub_oc_rc=0
stub_aws_rc=0

oc() {
    case "$*" in
        *providerID*)
            if (( stub_oc_rc != 0 )); then
                echo "error: You must be logged in to the server"
                return "${stub_oc_rc}"
            fi
            printf '%s' "${stub_provider_id}" ;;
        *) echo "unstubbed oc call: $*" >&2; return 1 ;;
    esac
}

aws() {
    case "$*" in
        *describe-instances*)
            if (( stub_aws_rc != 0 )); then
                echo "An error occurred (UnauthorizedOperation) when calling DescribeInstances"
                return "${stub_aws_rc}"
            fi
            # Only ever asked about the instance the node named.
            case "$*" in
                *"${stub_provider_id##*/}"*) printf '%s' "${stub_instance_vpc}" ;;
                *) printf 'None' ;;
            esac
            ;;
        *) echo "unstubbed aws call: $*" >&2; return 1 ;;
    esac
}

stub_provider_id="aws:///us-east-1d/i-0f5cb92a122a30d19"
stub_instance_vpc="vpc-0badc0ffee"
check "finds the VPC from a node's instance" \
    "$(aws_cluster_vpc)" "vpc-0badc0ffee"

# A cluster whose nodes have gone, or an instance EC2 no longer knows
# about, must fail loudly rather than return the empty string that every
# caller would then paste into a filter.
stub_instance_vpc="None"
(aws_cluster_vpc) >/dev/null 2>&1
check "fails when the instance has no VPC" "$?" "1"

stub_provider_id=""
(aws_cluster_vpc) >/dev/null 2>&1
check "fails when no node reports a providerID" "$?" "1"

# The callers set errexit as well as pipefail, and under both a pipeline
# whose grep matches nothing ends the script before die can name what was
# missing. The exit status is 1 either way, so only the message tells the
# two apart. This harness sets pipefail but not errexit, which is why
# these run in a subshell that sets what the callers set -- without it
# the check above passes over a diagnostic nobody will ever see.
out="$( ( set -o errexit; set -o pipefail; aws_cluster_vpc ) 2>&1 )"
check "names the missing providerID under the callers' options" \
    "$(printf '%s' "${out}" | grep -c 'no node reported a providerID')" "1"

# A cluster we cannot read is not a cluster without nodes, and saying so
# sends whoever reads the log looking in the wrong place. The callers run
# under errexit, so a command substitution that fails takes the script
# down before die can speak unless the failure is caught here.
stub_provider_id="aws:///us-east-1d/i-0f5cb92a122a30d19"
stub_instance_vpc="vpc-0badc0ffee"
stub_oc_rc=1
out="$( (aws_cluster_vpc) 2>&1 )"
check "fails when the cluster cannot be read" "$?" "1"
check "says the cluster could not be read" \
    "$(printf '%s' "${out}" | grep -c 'could not read the nodes')" "1"
check "repeats what oc said" \
    "$(printf '%s' "${out}" | grep -c 'must be logged in')" "1"
stub_oc_rc=0

stub_aws_rc=1
out="$( (aws_cluster_vpc) 2>&1 )"
check "fails when EC2 refuses the request" "$?" "1"
check "repeats what aws said" \
    "$(printf '%s' "${out}" | grep -c 'UnauthorizedOperation')" "1"
stub_aws_rc=0

unset -f oc aws

######################################################################
echo "--- aws/lib.sh: aws_cluster_node_subnets ---"

# One subnet per availability zone, for the route server endpoints.
# Asking for subnets tagged Name=*private* only works where whoever built
# the VPC named them that way: the terraform in rosa-bgp does, and the
# CloudFormation ci-operator uses does not tag subnets at all, so the
# filter matched nothing and the script stopped having done nothing.
#
# The fixtures are the shape of a real run: three nodes, one per zone,
# and the instance ids and subnet ids that run actually had.

stub_provider_ids=""
stub_oc_rc=0
stub_aws_rc=0

oc() {
    case "$*" in
        *providerID*)
            if (( stub_oc_rc != 0 )); then
                echo "error: You must be logged in to the server"
                return "${stub_oc_rc}"
            fi
            printf '%s' "${stub_provider_ids}" ;;
        *) echo "unstubbed oc call: $*" >&2; return 1 ;;
    esac
}

# Answers for the instances it was asked about and no others, so an
# instance the function fails to pass along shows up as a missing line
# rather than as output the stub invented.
aws() {
    local arg
    case "$*" in
        *describe-instances*)
            if (( stub_aws_rc != 0 )); then
                echo "An error occurred (UnauthorizedOperation) when calling DescribeInstances"
                return "${stub_aws_rc}"
            fi
            for arg in "$@"; do
                case "${arg}" in
                    i-081c16e05f2b3f713) echo "subnet-01fa033928a5bddd4	us-west-2a" ;;
                    i-0df8917b1589d18ac) echo "subnet-0791d165347551b4e	us-west-2b" ;;
                    i-07df699f97fbebb5e) echo "subnet-075f0f316b10ebc1c	us-west-2c" ;;
                    i-1|i-2)             echo "subnet-01fa033928a5bddd4	us-west-2a" ;;
                esac
            done ;;
        *) echo "unstubbed aws call: $*" >&2; return 1 ;;
    esac
}

stub_provider_ids="aws:///us-west-2a/i-081c16e05f2b3f713 aws:///us-west-2b/i-0df8917b1589d18ac aws:///us-west-2c/i-07df699f97fbebb5e"

check "returns a subnet and zone per node" \
    "$(aws_cluster_node_subnets | wc -l)" "3"
check "returns the zone alongside the subnet" \
    "$(aws_cluster_node_subnets | awk '$2=="us-west-2b"{print $1}')" "subnet-0791d165347551b4e"

# Several nodes in one zone is the ordinary case, and the caller takes
# one subnet per zone; this only has to report what it found.
stub_provider_ids="aws:///us-west-2a/i-1 aws:///us-west-2a/i-2"
check "reports a repeated zone rather than collapsing it" \
    "$(aws_cluster_node_subnets | wc -l)" "2"

# The failures, under the options the callers set rather than this
# harness's, because errexit is what decides whether die is reached.
stub_provider_ids=""
out="$( ( set -o errexit; set -o pipefail; aws_cluster_node_subnets ) 2>&1 )"
check "fails when no node reports a providerID" "$?" "1"
check "names the missing providerID" \
    "$(printf '%s' "${out}" | grep -c 'no node reported a providerID')" "1"

stub_provider_ids="aws:///us-west-2a/i-081c16e05f2b3f713"
stub_oc_rc=1
out="$( ( set -o errexit; set -o pipefail; aws_cluster_node_subnets ) 2>&1 )"
check "fails when the cluster cannot be read" "$?" "1"
check "repeats what oc said" \
    "$(printf '%s' "${out}" | grep -c 'must be logged in')" "1"
stub_oc_rc=0

stub_aws_rc=1
out="$( ( set -o errexit; set -o pipefail; aws_cluster_node_subnets ) 2>&1 )"
check "fails when EC2 refuses the request" "$?" "1"
check "repeats what aws said" \
    "$(printf '%s' "${out}" | grep -c 'UnauthorizedOperation')" "1"
stub_aws_rc=0

unset -f oc aws

echo "---"
echo "passed=${passed} failed=${failed}"
[[ "${failed}" -eq 0 ]]
