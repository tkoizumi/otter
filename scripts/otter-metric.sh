#!/bin/sh
# Publish a CloudWatch metric for the otter runtime host.
#
# heartbeat.sh and disk-check.sh use this to report. They used to ping an
# external dead-man service; a ping needed a URL, a service and a bearer
# secret, and none of those exist here any more. The replacement is a metric in
# the `Otter` namespace: while the host is healthy the check publishes, and
# while it is not the check publishes *nothing*. The alarm lives off the host,
# in the CDK stack that reads this namespace, and it fires on missing data -- so
# this script's whole job is to be the thing that goes quiet, and the check's
# job is to call it only when there is health to report.
#
# That inversion is why every failure here is fatal and loud on stderr. A
# missing IMDSv2 token, an unnamed instance role, credentials that do not parse,
# a 4xx/5xx from CloudWatch, a transport error -- each exits non-zero and
# publishes nothing. A caller that swallowed that would leave the alarm to fire
# on a host that is actually fine, or worse, to stay quiet on a host that is
# not; either way the journal of a silent host has to say which step broke.
#
# Credentials come from the instance itself, through IMDSv2, in this order:
#
#   1. PUT <imds>/latest/api/token with X-aws-ec2-metadata-token-ttl-seconds: 60
#      (the instance enforces IMDSv2, so every later read must carry the token)
#   2. GET <imds>/latest/meta-data/iam/security-credentials/  -> the role name
#   3. GET <imds>/latest/meta-data/iam/security-credentials/<role> -> the JSON
#      document holding AccessKeyId, SecretAccessKey and Token
#   4. the region: OTTER_METRIC_REGION, else
#      GET <imds>/latest/meta-data/placement/region, else us-east-1
#
# The JSON is read with sed, deliberately. This runs on a host that must need
# nothing new installed: no jq, no python, no AWS CLI. Nothing here reads or
# writes a credential file -- there is no long-lived key on the host at all.
#
# No credential value is ever printed, in an error or anywhere else. The
# access key, the secret and the session token travel only as curl arguments;
# the messages below name the *step* that failed, never its contents. That is
# also why nothing is traced: one `set -x` would put half a key in the journal.
# The instance role's *name* is not a secret and may appear, because an operator
# debugging a silent host needs to know which role was assumed.
#
#   OTTER_METRIC_HOST      the Host dimension value      (default castor-runtime)
#   OTTER_METRIC_TENANT    the Tenant dimension value    (default: no Tenant dimension)
#   OTTER_METRIC_REGION    the AWS region to sign for    (default: IMDS, else us-east-1)
#   OTTER_METRIC_ENDPOINT  the CloudWatch query endpoint (default below)
#   OTTER_IMDS_ENDPOINT    the metadata service          (default http://169.254.169.254)
#   OTTER_METRIC_TIMEOUT   seconds per request           (default 10)
#
# OTTER_METRIC_ENDPOINT and OTTER_IMDS_ENDPOINT exist so the drills and the
# tests can point both halves at local stand-ins; on a host neither is set.
#
#   otter-metric.sh <MetricName>=<Value> [<MetricName>=<Value> ...]
#
# Every metric becomes a member of one PutMetricData call, with
# StorageResolution=60 so a 1-minute alarm period has datapoints to read.
#
# Exit status: 0 means CloudWatch accepted the call. Non-zero means nothing was
# published, or the publish failed; in both cases the caller must treat the host
# as unreported.
set -eu

die() {
	echo "otter-metric: $1" >&2
	exit 1
}

host=${OTTER_METRIC_HOST:-castor-runtime}
# A per-tenant publisher sets this so the datapoint carries a `Tenant` dimension
# BESIDE the host-wide `Host` one. It is a dimension VALUE, so it is validated
# here rather than trusted: a newline or a quote in a metric request is how a
# request is reshaped.
tenant=${OTTER_METRIC_TENANT:-}
case $tenant in
"") ;;
*[!A-Za-z0-9_.-]*) die "OTTER_METRIC_TENANT '$tenant' is not a plain dimension value" ;;
esac
timeout=${OTTER_METRIC_TIMEOUT:-10}
imds=${OTTER_IMDS_ENDPOINT:-http://169.254.169.254}
imds=${imds%/}
region=${OTTER_METRIC_REGION:-}

# The metric pairs are validated and rebuilt as curl's own argument list before
# any credential is fetched. A typo in the command line is a usage error that
# needs no instance role to report, and a request built from a bad pair must
# never reach the point of signing.
#
# POSIX sh has no arrays, so the pairs are moved through a here-doc and the
# positional parameters are reused: no value ever travels through an unquoted
# expansion, which is what keeps a value from being read as a second option.
[ "$#" -gt 0 ] ||
	die "no metrics given; usage: otter-metric.sh <MetricName>=<Value> [<MetricName>=<Value> ...]"

pairs=$(printf '%s\n' "$@")
set --
member=0
while IFS= read -r pair; do
	[ -n "$pair" ] || continue
	case $pair in
	*=*) ;;
	*) die "metric '$pair' is not of the form <MetricName>=<Value>" ;;
	esac
	name=${pair%%=*}
	value=${pair#*=}
	case $name in
	"" | *[!A-Za-z0-9_.-]*)
		die "metric name '$name' is empty or is not a plain metric name"
		;;
	esac
	case $value in
	"" | *[!0-9.eE+-]*)
		die "value '$value' for metric '$name' is not a number"
		;;
	*[0-9]*) ;;
	*) die "value '$value' for metric '$name' is not a number" ;;
	esac
	member=$((member + 1))
	set -- "$@" \
		--data-urlencode "MetricData.member.$member.MetricName=$name" \
		--data-urlencode "MetricData.member.$member.Value=$value" \
		--data-urlencode "MetricData.member.$member.Dimensions.member.1.Name=Host" \
		--data-urlencode "MetricData.member.$member.Dimensions.member.1.Value=$host"
	if [ -n "$tenant" ]; then
		set -- "$@" \
			--data-urlencode "MetricData.member.$member.Dimensions.member.2.Name=Tenant" \
			--data-urlencode "MetricData.member.$member.Dimensions.member.2.Value=$tenant"
	fi
	set -- "$@" \
		--data-urlencode "MetricData.member.$member.StorageResolution=60"
done <<EOF
$pairs
EOF

[ "$member" -gt 0 ] || die "no metrics given"

# json_string prints the value of a top-level string member, or nothing. IMDS
# emits one flat JSON object; a greedy match up to the key and a non-greedy
# value is exact for these fields, which never contain an unescaped quote.
json_string() { # json, key
	printf '%s\n' "$1" |
		sed -n 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
		head -n 1
}

# 1. The IMDSv2 token. Without this PUT the instance refuses to hand over the
# token at all, and the TTL header is what IMDSv2 requires on the request.
token=$(curl -fsS --max-time "$timeout" -X PUT \
	-H "X-aws-ec2-metadata-token-ttl-seconds: 60" \
	"$imds/latest/api/token") ||
	die "cannot get an IMDSv2 token from $imds/latest/api/token"
[ -n "$token" ] ||
	die "IMDSv2 returned an empty token from $imds/latest/api/token"

# Every later read carries the token. A read without it is a 401 on a real
# instance, which is why there is no unauthenticated fallback here.
imds_get() { # path
	curl -fsS --max-time "$timeout" \
		-H "X-aws-ec2-metadata-token: $token" \
		"$imds$1"
}

# 2. The role name. An instance with no role answers this read with a 404 or an
# empty body; both mean the same thing here.
role=$(imds_get "/latest/meta-data/iam/security-credentials/") ||
	die "cannot read the instance role name from $imds/latest/meta-data/iam/security-credentials/"
[ -n "$role" ] ||
	die "IMDS returned an empty role name; this instance has no IAM role attached"
case $role in
*[!A-Za-z0-9+=,.@_-]*)
	die "IMDS returned a role name that is not a plain role name; refusing to use it"
	;;
esac

# 3. The role's credentials.
creds=$(imds_get "/latest/meta-data/iam/security-credentials/$role") ||
	die "cannot read credentials for role '$role' from IMDS"

ak=$(json_string "$creds" AccessKeyId)
sk=$(json_string "$creds" SecretAccessKey)
session=$(json_string "$creds" Token)
[ -n "$ak" ] ||
	die "the credentials for role '$role' carry no AccessKeyId"
[ -n "$sk" ] ||
	die "the credentials for role '$role' carry no SecretAccessKey"
[ -n "$session" ] ||
	die "the credentials for role '$role' carry no Token"

# 4. The region. The one IMDS read that may fail without failing the publish:
# an instance outside a region-less environment always answers it, but a
# stand-in or a stripped metadata service may not, and us-east-1 is the
# documented fallback rather than an error.
if [ -z "$region" ]; then
	region=$(imds_get "/latest/meta-data/placement/region") || region=
fi
region=${region:-us-east-1}

endpoint=${OTTER_METRIC_ENDPOINT:-https://monitoring.$region.amazonaws.com}

# curl signs the request itself (curl 8.5 on Ubuntu 24.04 supports this natively
# as far back as 7.75), so the host needs no aws CLI and no SDK. -f turns any
# 4xx/5xx into a non-zero exit; -sS keeps the progress meter quiet but lets the
# transport error reach stderr, where it names the endpoint and never the key.
if ! curl -fsS -o /dev/null --max-time "$timeout" \
	--aws-sigv4 "aws:amz:$region:monitoring" \
	--user "$ak:$sk" \
	-H "x-amz-security-token: $session" \
	-H "Content-Type: application/x-www-form-urlencoded" \
	--data-urlencode "Action=PutMetricData" \
	--data-urlencode "Version=2010-08-01" \
	--data-urlencode "Namespace=Otter" \
	"$@" \
	"$endpoint"; then
	die "PutMetricData to $endpoint failed; nothing was published"
fi
