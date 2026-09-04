#!/usr/bin/env bash
#
# Create the AWS VPC Route Server estate a BGPCloudConfiguration expects to
# discover.
#
# The operator discovers route servers and endpoints; it never creates
# them. It creates only the peers, and flips SourceDestCheck on the
# router nodes. So this fills the gap: one route server, endpoints in
# one private subnet per availability zone, associated with the
# cluster's VPC, propagating onto every route table.
#
#   hack/aws/create-route-servers.sh --dry-run
#   hack/aws/create-route-servers.sh
#
# Needs AWS credentials for the account owning the cluster, e.g.
# AWS_PROFILE=saml-refresh, and a KUBECONFIG: the infra id and region
# come from the running cluster rather than from arguments.
#
# What this builds tracks rh-mobb/rosa-bgp's vpc1-rs1.tf, because a
# cluster built by hand and one built by that Terraform should present
# the operator with the same estate. That means two endpoints per subnet
# rather than one, and propagation onto every route table.
#
# Propagation is the one that hides. Without it every peer reaches
# available, every BGP session establishes, FRR advertises the CUDN
# prefix, and the routes stay inside the route server: nothing in the
# VPC can reach a pod while every signal says healthy. Set
# ENDPOINTS_PER_SUBNET=1 for a cheaper throwaway, knowing it halves the
# redundancy the reference has.
#
# ASN sets the Amazon-side ASN and must differ from the localASN in your
# BGPCloudConfiguration, or the session is not eBGP.
#
# Rerunning adopts what already exists rather than creating a second
# set, so it is safe as a check on the current state.
#
# Endpoints bill hourly and belong to the VPC rather than the cluster,
# so nothing else reclaims them. Tear them down with
# hack/aws/delete-route-servers.sh.

set -o nounset
set -o errexit
set -o pipefail

# shellcheck source=hack/aws/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

asn="${ASN:-65000}"
endpoints_per_subnet="${ENDPOINTS_PER_SUBNET:-2}"

# Checked here rather than discovered afterwards. Zero satisfies the
# "already has enough" comparison below for every subnet, so the loop
# creates nothing and the script reports OK on a route server that no
# peer can attach to. A non-number does the same, since bash reads it as
# the name of an unset variable and calls it zero, and so does anything
# that wraps: 2^64 is all digits and evaluates to zero too.
#
# So the range is matched, not computed. Nothing here reaches bash
# arithmetic until it is known to be one or two digits, and a route
# server takes sixteen peers in total, so sixteen per subnet is already
# past anything useful.
[[ "${endpoints_per_subnet}" =~ ^([1-9]|1[0-6])$ ]] \
    || die "ENDPOINTS_PER_SUBNET must be an integer between 1 and 16, got '${endpoints_per_subnet}'"

parse_args "$@"
require_cmd aws oc
require_cluster
require_platform AWS
require_aws

aws_cluster_facts
require_route_server_api
vpc="$(aws_cluster_vpc)"

info "cluster:  ${infra}"
info "region:   ${region}"
info "vpc:      ${vpc}"
info "asn:      ${asn}"

# Reusing an ASN is legal and usually harmless: a route server only
# peers inside its own VPC, so two clusters in two VPCs never see each
# other's advertisements. It stops being harmless the moment those VPCs
# are peered or joined through a Transit Gateway, because BGP discards
# any route whose AS_PATH already contains the receiver's own ASN. Two
# clusters on one ASN would then drop each other's routes silently,
# which is a miserable thing to debug months later.
#
# So warn and continue rather than refuse. You may well mean it.
clash_all="$(aws_query "list route servers on ASN ${asn}" \
    aws ec2 describe-route-servers \
    --query "RouteServers[?State!='deleted' && AmazonSideAsn==\`${asn}\`].[RouteServerId,Tags[?Key=='Name']|[0].Value]" \
    --output text)" || die "cannot check whether ASN ${asn} is already in use"
clash=""
while IFS= read -r line; do
    [[ -z "${line}" || "${line}" == *"${infra}"* ]] && continue
    clash+="${line}"$'\n'
done <<<"${clash_all}"
if [[ -n "${clash}" ]]; then
    warn "ASN ${asn} is already used by another route server in ${region}:"
    printf '%s\n' "${clash}" | sed 's/^/  /' >&2
    warn "Fine while the VPCs stay separate. If they are ever peered or"
    warn "joined by a Transit Gateway, routes will be silently discarded."
    warn "Set ASN=<other> to avoid it. The private range is 64512-65534,"
    warn "and it must differ from localASN in your BGPCloudConfiguration."
fi

# One endpoint per AZ, so take the first private subnet in each.
#
# Read before the loop rather than piped into it. At the head of a
# pipeline the describe's exit status is the sort's, and a call that
# failed then arrives as no output, which the check below reports as
# "no private subnets" -- sending you to look at the VPC rather than at
# the API call that did not answer.
subnets_raw="$(aws_query "list private subnets in ${vpc}" \
    aws ec2 describe-subnets \
    --filters "Name=vpc-id,Values=${vpc}" "Name=tag:Name,Values=*private*" \
    --query 'Subnets[].[SubnetId,AvailabilityZone]' --output text)" \
    || die "cannot list the subnets in ${vpc}"

subnets=()
seen_azs=""
while read -r subnet az; do
    [[ -n "${subnet}" ]] || continue
    case " ${seen_azs} " in *" ${az} "*) continue ;; esac
    seen_azs="${seen_azs} ${az}"
    subnets+=("${subnet}:${az}")
done < <(sort -k2 <<<"${subnets_raw}")

(( ${#subnets[@]} > 0 )) || die "no private subnets found in ${vpc}"

info "private subnets, one per AZ:"
for entry in "${subnets[@]}"; do
    info "  ${entry#*:}  ${entry%%:*}"
done

rs="$(route_server_for_cluster "${infra}")"

route_server_available() {
    local state
    state="$(aws ec2 describe-route-servers --route-server-ids "$1" \
        --query 'RouteServers[0].State' --output text 2>/dev/null)" || return 1
    [[ "${state}" == "available" ]]
}

if [[ -n "${rs}" ]]; then
    info "OK   adopting route server ${rs}"
elif [[ "${dry_run}" == true ]]; then
    info "  would run: aws ec2 create-route-server --amazon-side-asn ${asn}"
    rs="rs-DRYRUN"
else
    rs="$(aws ec2 create-route-server \
        --amazon-side-asn "${asn}" \
        --tag-specifications "ResourceType=route-server,Tags=[{Key=Name,Value=${infra}-rs}]" \
        --query 'RouteServer.RouteServerId' --output text)"
    info "OK   create-route-server ${rs}"
    wait_until 600 10 "route server to become available" route_server_available "${rs}" \
        || die "route server ${rs} never became available"
fi

associated=false
if [[ "${rs}" != "rs-DRYRUN" ]]; then
    # Captured before matching. With route_server_vpcs at the head of a
    # pipeline its failure reaches only that stage's subshell, the match
    # then finds nothing, and a read that failed reads as "not
    # associated".
    assoc_vpcs="$(route_server_vpcs "${rs}")" \
        || die "cannot tell whether ${rs} is already associated with ${vpc}"
    for v in ${assoc_vpcs}; do
        [[ "${v}" == "${vpc}" ]] && associated=true
    done
fi

if [[ "${associated}" == true ]]; then
    info "OK   already associated with ${vpc}"
else
    aws_retry "associate with ${vpc}" 300 \
        aws ec2 associate-route-server --route-server-id "${rs}" --vpc-id "${vpc}"
    ok "associate-route-server ${vpc}"
fi

for entry in "${subnets[@]}"; do
    subnet="${entry%%:*}"
    az="${entry#*:}"

    # A tombstone is excluded like the route server adoption above:
    # adopting one means "already exists" for an endpoint that never
    # gets recreated.
    if [[ "${rs}" == "rs-DRYRUN" ]]; then
        have=0
    else
        have="$(live_endpoints_in_subnet "${rs}" "${subnet}")"
    fi

    if (( have >= endpoints_per_subnet )); then
        info "OK   ${az} has ${have} endpoint(s) in ${subnet}"
        continue
    fi

    for (( n = have + 1; n <= endpoints_per_subnet; n++ )); do
        aws_retry "create endpoint ${n} in ${subnet}" 300 \
            aws ec2 create-route-server-endpoint \
            --route-server-id "${rs}" \
            --subnet-id "${subnet}" \
            --tag-specifications "ResourceType=route-server-endpoint,Tags=[{Key=Name,Value=${infra}-rs-${az}-ep${n}}]"
        ok "create-route-server-endpoint ${az} ${n}/${endpoints_per_subnet} in ${subnet}"
    done
done

# Propagation onto the route tables. The Terraform covers private and
# public alike, so this does too: a route server that peers but
# propagates nowhere is the failure this whole script exists to avoid,
# and it is invisible from the cluster.
# A failed read here used to warn "nothing to propagate to" and carry
# on, which builds the whole estate with no propagation and reports it
# as a success -- the silent failure this script exists to prevent.
route_tables="$(vpc_route_tables "${vpc}")" \
    || die "cannot list the route tables in ${vpc}"

if [[ -z "${route_tables}" ]]; then
    die "no route tables found in ${vpc}" \
        "A route server that propagates nowhere peers and establishes and" \
        "advertises, while nothing in the VPC can reach a pod."
else
    if [[ "${rs}" == "rs-DRYRUN" ]]; then
        propagated=""
    else
        propagated="$(route_server_propagations "${rs}")" \
            || die "cannot read the propagations for ${rs}"
    fi

    for rt in ${route_tables}; do
        if printf '%s\n' "${propagated}" | grep -qx "${rt}"; then
            info "OK   ${rt} already propagating"
            continue
        fi
        aws_retry "enable propagation to ${rt}" 300 \
            aws ec2 enable-route-server-propagation \
            --route-server-id "${rs}" --route-table-id "${rt}"
        ok "enable-route-server-propagation ${rt}"
    done
fi

if [[ "${dry_run}" == true ]]; then
    info "dry run only, nothing was created"
    exit 0
fi

# Predicate for wait_until. Deleted tombstones are excluded: AWS keeps
# returning them, and counting one as not-yet-available would make a
# rerun wait out the whole timeout on an endpoint nothing can revive. An
# API failure is a failed poll, not an empty answer.
endpoints_available() {
    local pending
    # The same live/tombstone definition as live_endpoints_in_subnet. An
    # endpoint left in deleting by an interrupted teardown is invisible
    # to the adoption count, so counting it as pending here would poll a
    # resource nothing can revive for the whole budget.
    pending="$(aws ec2 describe-route-server-endpoints \
        --query "length(RouteServerEndpoints[?RouteServerId=='$1' && State!='available' && State!='deleted' && State!='deleting'])" \
        --output text 2>/dev/null)" || return 1
    [[ "${pending}" == "0" ]]
}

# The endpoint records carry no AvailabilityZone, only SubnetId.
print_endpoints() {
    aws ec2 describe-route-server-endpoints \
        --query "RouteServerEndpoints[?RouteServerId=='${rs}'].[SubnetId,RouteServerEndpointId,EniAddress,State]" \
        --output table
}

if ! wait_until 600 10 "endpoints to become available" endpoints_available "${rs}"; then
    print_endpoints
    die "endpoints are not all available; their states are in the table above" \
        "Endpoints bill hourly. If you are abandoning this, tear them down:" \
        "hack/aws/delete-route-servers.sh"
fi

print_endpoints

cat <<EOF
Put this in your BGPCloudConfiguration:

  spec:
    platform: AWS
    aws:
      region: ${region}
      routeServerIDs:
        - ${rs}

platform and the cloud block have to agree: a CEL rule requires spec.aws
when the platform is AWS and rejects it on any other, so neither half is
optional.
EOF
