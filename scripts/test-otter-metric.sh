#!/bin/sh
# Falsifiability for scripts/otter-metric.sh.
#
# otter-metric.sh is the publisher the two host checks call. It talks to two
# services at once -- the IMDSv2 metadata service for credentials and the
# CloudWatch query endpoint for the metric -- and each is stood in for here by
# its own local HTTP server on its own port, so a publisher that sent its metric
# to the metadata service cannot land somewhere that accepts it. Both record
# every request they receive. The point is not to test HTTP or SigV4; it is to
# prove that *this script*:
#
#   * publishes one request carrying the namespace, the Host dimension and
#     StorageResolution=60, and puts several metrics in that one request;
#   * signs that request: an AWS4-HMAC-SHA256 Authorization header whose
#     credential scope names the region it was configured to sign for, and the
#     session token IMDS issued -- all read back off the wire, not assumed;
#   * refuses to publish, non-zero and loud, when IMDS will not issue a token,
#     when the instance role has no name, or when the credentials do not parse;
#   * treats a non-2xx from the endpoint as failure;
#   * validates its command line before touching IMDS at all;
#   * and never prints a credential value, on success or on failure.
#
# The signature half of that is what an audit found missing: with a receiver
# that accepted any request and recorded only method, path and body, the suite
# stayed green with the publisher completely unsigned. The CloudWatch stand-in
# therefore answers an unsigned POST the way CloudWatch does -- 403,
# MissingAuthenticationToken -- and one case below points curl at it directly,
# to prove the fixture itself refuses what it claims to refuse. A fixture that
# accepts anything is not a fixture.
#
# The IMDS stand-in enforces the IMDSv2 sequence the way the instance does: the
# token PUT needs its TTL header, and every /latest/ read needs the token. A
# publisher that skipped either would fail the positive cases instead of quietly
# working against a permissive fixture.
#
# The credentials the stand-in hands out are canaries. "Canary" here means a
# value chosen to be greppable, not a real key: nothing in this suite reaches
# AWS or 169.254.169.254.
#
#   sh scripts/test-otter-metric.sh
#
# OTTER_METRIC_SUBJECT points the matrix at a mutated copy of the script, the
# same way scripts/test-provision.sh does.
set -eu

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
subject=${OTTER_METRIC_SUBJECT:-$here/otter-metric.sh}

[ -f "$subject" ] || {
	echo "test-otter-metric: missing $subject" >&2
	exit 2
}

unset OTTER_METRIC_ENDPOINT OTTER_METRIC_REGION OTTER_METRIC_HOST \
	OTTER_METRIC_TIMEOUT OTTER_IMDS_ENDPOINT 2>/dev/null || true

work=$(mktemp -d "${TMPDIR:-/tmp}/otter-metric-test.XXXXXX")
receiver_pid=""
imds_pid=""
cleaning=0
passed=0
failed=0

# fatal is for a fixture or setup problem: the matrix cannot run, so it must not
# report a verdict at all. fail is for a case that ran and was wrong.
fatal() {
	echo "test-otter-metric: $1" >&2
	exit 2
}

fail() {
	failed=$((failed + 1))
	echo "test-otter-metric: FAIL: $1" >&2
}

show() {
	printf '%s\n' "$1" | sed 's/^/    /' >&2
}

ok() {
	passed=$((passed + 1))
	echo "ok: $1"
}

cleanup() {
	status=$?
	[ "$cleaning" -eq 1 ] && return
	cleaning=1
	[ -n "$receiver_pid" ] && kill "$receiver_pid" 2>/dev/null || true
	[ -n "$imds_pid" ] && kill "$imds_pid" 2>/dev/null || true
	if [ "$status" -ne 0 ] || [ "$failed" -ne 0 ]; then
		echo "test-otter-metric: workspace kept at $work" >&2
	else
		rm -rf "$work"
	fi
}
trap cleanup EXIT HUP INT TERM

# --- the stand-in IMDSv2 and CloudWatch --------------------------------------
# Two servers with the same code and a role argument: the cloudwatch instance
# serves POST / and refuses the metadata reads, and the imds instance serves the
# token PUT and the /latest/ reads and refuses a POST. Neither behaviour is read
# from the role file; the modes below change only how a healthy host fails. The
# mode file is read on every request, so a case changes the fixture by rewriting
# one file rather than restarting a listener. The default is a fully healthy
# host.
CANARY_AK=AKIA-CANARY-ACCESS-KEY-7f3a9c
CANARY_SK=CANARY-SECRET-ACCESS-KEY-9c1d4b
CANARY_TOKEN=CANARY-SESSION-TOKEN-4b2e8e
mode=$work/mode
log=$work/cloudwatch.log
imds_log=$work/imds.log

cat >"$work/receiver.py" <<'PY'
import http.server, json, os, sys

port = int(sys.argv[1])
role = sys.argv[2]
log = os.environ["OTTER_STANDIN_LOG"]
mode_file = os.environ.get("OTTER_STANDIN_MODE", "")
role_name = "otter-metric-role"
region = "us-east-1"
ak = os.environ.get("OTTER_STANDIN_AK", "AKIA-STANDIN-ACCESS-KEY")
sk = os.environ.get("OTTER_STANDIN_SK", "standin-secret-access-key")
session = os.environ.get("OTTER_STANDIN_SESSION", "standin-session-token")

# What CloudWatch answers a call it could not authenticate. The publisher never
# reads this body -- curl -f only needs the status -- but it is asserted on
# below, so that the fixture is seen to reject for the documented reason and not
# merely to reject.
MISSING_AUTH_TOKEN = (
    b'<?xml version="1.0" encoding="UTF-8"?>'
    b'<ErrorResponse xmlns="http://monitoring.amazonaws.com/doc/2010-08-01/">'
    b"<Error><Type>Sender</Type><Code>MissingAuthenticationToken</Code>"
    b"<Message>Request is missing Authentication Token</Message></Error>"
    b"</ErrorResponse>"
)


def mode(key, default):
    try:
        with open(mode_file) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#"):
                    continue
                k, _, v = line.partition("=")
                if k.strip() == key:
                    return v.strip()
    except OSError:
        pass
    return default


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def body(self):
        try:
            n = int(self.headers.get("Content-Length", "0") or 0)
        except ValueError:
            n = 0
        return self.rfile.read(n) if n > 0 else b""

    def record(self, payload=b""):
        # One line per fact. The request line keeps the "METHOD path" shape
        # count_posts and the path assertion grep for; a POST's two signing
        # headers go on their own lines behind a tag, so a body field that
        # happens to hold the same bytes cannot satisfy a claim about the
        # signature.
        with open(log, "a") as f:
            f.write("%s %s\n" % (self.command, self.path))
            if self.command == "POST":
                f.write("[auth] %s\n" % (self.headers.get("Authorization") or "(absent)"))
                f.write("[token] %s\n" % (self.headers.get("X-Amz-Security-Token") or "(absent)"))
            if payload:
                f.write(payload.decode("utf-8", "replace") + "\n")

    def signed(self):
        # CloudWatch wants both halves: a SigV4 Authorization header and the
        # session token that goes with the credential. Missing either is the
        # MissingAuthenticationToken case below.
        return (
            self.headers.get("Authorization", "").startswith("AWS4-HMAC-SHA256 ")
            and bool(self.headers.get("X-Amz-Security-Token"))
        )

    def reply(self, status, payload=b"", ctype="text/plain"):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if payload:
            self.wfile.write(payload)

    def do_PUT(self):
        # Only the metadata service has a token endpoint.
        if role != "imds":
            self.record()
            self.reply(404)
            return
        self.record()
        if self.path == "/latest/api/token" and \
                self.headers.get("X-aws-ec2-metadata-token-ttl-seconds"):
            if mode("token", "ok") != "ok":
                self.reply(404)
                return
            self.reply(200, b"standin-imds-token")
            return
        self.reply(400)

    def do_GET(self):
        if self.path.split("?")[0] == "/__ready":
            # A readiness probe is not a request under test. Answer with the
            # role, and record nothing, so the log holds only what the
            # publisher sent.
            self.reply(200, role.encode())
            return
        # Only the metadata service serves the /latest/ reads.
        if role != "imds":
            self.record()
            self.reply(404)
            return
        self.record()
        path = self.path.split("?")[0]
        if not self.headers.get("X-aws-ec2-metadata-token"):
            self.reply(401)
            return
        if path == "/latest/meta-data/placement/region":
            self.reply(200, region.encode())
            return
        if path == "/latest/meta-data/iam/security-credentials/":
            if mode("role", "ok") != "ok":
                self.reply(200, b"")
                return
            self.reply(200, role_name.encode())
            return
        if path.startswith("/latest/meta-data/iam/security-credentials/"):
            if mode("creds", "ok") != "ok":
                self.reply(404)
                return
            self.reply(200, json.dumps({
                "Code": "Success",
                "AccessKeyId": ak,
                "SecretAccessKey": sk,
                "Token": session,
                "Expiration": "2030-01-01T00:00:00Z",
            }).encode(), "application/json")
            return
        self.reply(404)

    def do_POST(self):
        payload = self.body()
        # The metadata service has no publish endpoint at all. A publisher
        # whose metric URL was pointed here gets a rejection, not a metric.
        if role != "cloudwatch":
            self.record()
            self.reply(405, b"<ErrorResponse><Error><Code>MethodNotAllowed</Code>"
                            b"</Error></ErrorResponse>", "text/xml")
            return
        if not self.signed():
            # Rejected before the body is recorded: an unauthenticated request
            # is not a publish, and the suite asserts that it leaves no metric
            # body behind. The headers are still recorded, so a failed
            # assertion can show which half of the signature was missing.
            self.record()
            self.reply(403, MISSING_AUTH_TOKEN, "text/xml")
            return
        self.record(payload)
        try:
            status = int(mode("post", "200"))
        except ValueError:
            status = 200
        if 200 <= status < 300:
            self.reply(status, b"<PutMetricDataResponse/>", "text/xml")
        else:
            self.reply(status, b"<ErrorResponse/>", "text/xml")


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PY

free_port() {
	python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

# write_mode <token> <role> <creds> <post>
write_mode() {
	{
		printf 'token=%s\n' "$1"
		printf 'role=%s\n' "$2"
		printf 'creds=%s\n' "$3"
		printf 'post=%s\n' "$4"
	} >"$mode"
}

write_mode ok ok ok 200

# Two listeners, one per service, each told which role it plays. free_port
# closes the socket it probed before returning, so a second call can in
# principle hand back the same port; loop until the two differ.
metric_port=$(free_port)
imds_port=$(free_port)
while [ "$imds_port" = "$metric_port" ]; do
	imds_port=$(free_port)
done

OTTER_STANDIN_LOG=$log OTTER_STANDIN_MODE=$mode \
	OTTER_STANDIN_AK=$CANARY_AK OTTER_STANDIN_SK=$CANARY_SK \
	OTTER_STANDIN_SESSION=$CANARY_TOKEN \
	python3 "$work/receiver.py" "$metric_port" cloudwatch \
	>"$work/cloudwatch.out" 2>&1 &
receiver_pid=$!

OTTER_STANDIN_LOG=$imds_log OTTER_STANDIN_MODE=$mode \
	OTTER_STANDIN_AK=$CANARY_AK OTTER_STANDIN_SK=$CANARY_SK \
	OTTER_STANDIN_SESSION=$CANARY_TOKEN \
	python3 "$work/receiver.py" "$imds_port" imds \
	>"$work/imds.out" 2>&1 &
imds_pid=$!

METRIC_URL="http://127.0.0.1:$metric_port"
IMDS_URL="http://127.0.0.1:$imds_port"
ready=0
for _ in $(seq 1 50); do
	if curl -s -o /dev/null --max-time 1 "$METRIC_URL/__ready" &&
		curl -s -o /dev/null --max-time 1 "$IMDS_URL/__ready"; then
		ready=1
		break
	fi
	sleep 0.2
done
[ "$ready" -eq 1 ] || fatal "the stand-in endpoints did not start"

count_posts() {
	count=$(grep -c '^POST ' "$log" 2>/dev/null || true)
	echo "${count:-0}"
}

run_subject() {
	OTTER_METRIC_ENDPOINT=$METRIC_URL \
		OTTER_IMDS_ENDPOINT=$IMDS_URL \
		OTTER_METRIC_TIMEOUT=5 \
		sh "$subject" "$@" 2>&1
}

# expect NAME ok|fail one|none SUBSTRING [MetricName=Value ...]
#
# `one` means the receiver must log exactly one POST; `none` means none at all.
# Both halves matter: a publisher that failed for the wrong reason, or that sent
# a request it was supposed to refuse, is not the behaviour the alarm reads.
expect() {
	name=$1
	want=$2
	posts=$3
	must=$4
	shift 4

	: >"$log"
	: >"$imds_log"
	status=0
	out=$(run_subject "$@") || status=$?
	got=$(count_posts)

	if [ "$want" = ok ] && [ "$status" -ne 0 ]; then
		fail "$name: want success, got exit $status"
		show "$out"
		return
	fi
	if [ "$want" = fail ] && [ "$status" -eq 0 ]; then
		fail "$name: want failure, got success"
		show "$out"
		return
	fi
	if [ "$posts" = one ] && [ "$got" -ne 1 ]; then
		fail "$name: want exactly one publish, the receiver saw $got"
		show "$out"
		return
	fi
	if [ "$posts" = none ] && [ "$got" -ne 0 ]; then
		fail "$name: want no publish, the receiver saw $got"
		show "$out"
		return
	fi
	if [ -n "$must" ] && ! printf '%s\n' "$out" | grep -Fq "$must"; then
		fail "$name: output does not contain the expected text"
		echo "    want substring: $must" >&2
		show "$out"
		return
	fi
	ok "$name"
}

# expect_no_canary NAME [MetricName=Value ...] runs the subject and asserts that
# none of the three credential values the stand-in handed out appears in the
# combined stdout/stderr, whatever the exit status was.
expect_no_canary() {
	name=$1
	shift

	: >"$log"
	: >"$imds_log"
	status=0
	out=$(run_subject "$@") || status=$?
	leaked=
	for secret in "$CANARY_AK" "$CANARY_SK" "$CANARY_TOKEN"; do
		if printf '%s\n' "$out" | grep -Fq "$secret"; then
			leaked=$secret
			break
		fi
	done
	if [ -n "$leaked" ]; then
		fail "$name: a credential value appeared in the output"
		show "$out"
		return
	fi
	ok "$name (exit $status)"
}

# assert_field NAME KEY VALUE reads the form body the CloudWatch stand-in
# recorded. It matches the URL-encoded body only at field boundaries, because
# an unanchored substring is not enough for a body field: "Otter" also matches
# "OtterMetricsX", "60" also matches "600", "Host" also matches "HostName", and
# "1" also matches "14200". A dot in a key is still a regex wildcard, but the
# (&|$) boundary either side is what the assertion rests on.
assert_field() {
	if grep -Eq "(^|&)$2=$3(&|\$)" "$log"; then
		ok "$1"
	else
		fail "$1: the published form body has no $2=$3 field"
		sed 's/^/    /' "$log" >&2
	fi
}

# assert_header NAME TAG SUBSTRING reads one of the header lines the receiver
# wrote for a publish: "[auth] " for Authorization, "[token] " for
# X-Amz-Security-Token. Scoping to the tagged line is deliberate -- a body field
# or a different header that happens to contain the same bytes must not be able
# to satisfy a claim about the signature.
assert_header() {
	if grep -Eq "^\[$2\] .*$3" "$log"; then
		ok "$1"
	else
		fail "$1: no [$2] header carrying '$3' was recorded"
		sed 's/^/    /' "$log" >&2
	fi
}

# assert_token NAME asserts that the X-Amz-Security-Token the publish carried is
# exactly the session token the IMDS stand-in issued. Exact, not a substring: a
# token that merely starts the same way, or an empty one, must not pass.
assert_token() {
	got=$(grep '^\[token\] ' "$log" 2>/dev/null | head -n 1 | sed 's/^\[token\] //' || true)
	if [ "$got" = "$2" ]; then
		ok "$1"
	else
		fail "$1: the publish did not carry the session token IMDS issued"
		sed 's/^/    /' "$log" >&2
	fi
}

# assert_imds NAME SUBSTRING is the plain whole-line substring helper, kept for
# the one assertion that is a request path rather than a form field: the region
# read, "GET /latest/meta-data/placement/region". It reads what the metadata
# stand-in saw, never the metric stand-in.
assert_imds() {
	if grep -Fq "$2" "$imds_log"; then
		ok "$1"
	else
		fail "$1: the metadata stand-in never saw '$2'"
		sed 's/^/    /' "$imds_log" >&2
	fi
}

# --- the fixture refuses what CloudWatch refuses ------------------------------
# Every signature assertion below rests on the stand-in refusing an unsigned
# call, and a receiver that accepts anything is exactly what made the old suite
# unfalsifiable. So assert the fixture first, with a request the publisher would
# never send: a bare POST carrying a metric body but neither an Authorization
# header nor a session token.
: >"$log"
: >"$imds_log"
unsigned_code=$(curl -s -o "$work/unsigned-body" -w '%{http_code}' --max-time 5 \
	-X POST -H "Content-Type: application/x-www-form-urlencoded" \
	--data-urlencode "Action=PutMetricData" \
	--data-urlencode "Namespace=Otter" \
	"$METRIC_URL/") || unsigned_code=000
if [ "$unsigned_code" = 403 ]; then
	ok "the metric stand-in rejects an unsigned POST with 403"
else
	fail "the metric stand-in answered an unsigned POST with $unsigned_code, not 403"
fi
if grep -Fq "MissingAuthenticationToken" "$work/unsigned-body"; then
	ok "the rejection names MissingAuthenticationToken, as CloudWatch does"
else
	fail "the rejection body does not name MissingAuthenticationToken"
	show "$(cat "$work/unsigned-body")"
fi
if grep -Fq "Action=PutMetricData" "$log"; then
	fail "the metric stand-in recorded the metric body of an unsigned POST"
	sed 's/^/    /' "$log" >&2
else
	ok "the metric stand-in records no metric body for an unsigned POST"
fi

# --- the positive control -----------------------------------------------------
# Everything the contract names about the request itself, asserted against the
# bytes the receiver actually logged.
expect "a live host publishes Heartbeat" ok one "" Heartbeat=1
assert_field "the namespace is Otter" Namespace Otter
assert_field "the action is PutMetricData" Action PutMetricData
assert_field "the version is 2010-08-01" Version 2010-08-01
assert_field "the metric name is carried" MetricData.member.1.MetricName Heartbeat
assert_field "the metric value is carried" MetricData.member.1.Value 1
assert_field "the dimension name is Host" MetricData.member.1.Dimensions.member.1.Name Host
assert_field "the dimension defaults to castor-runtime" MetricData.member.1.Dimensions.member.1.Value castor-runtime
assert_field "the storage resolution is 60 seconds" MetricData.member.1.StorageResolution 60

# A perfect body still buys nothing if CloudWatch cannot authenticate the call,
# so assert the signature itself off the recorded headers: the algorithm in the
# Authorization header, the region in its credential scope, and the session
# token IMDS issued. No OTTER_METRIC_REGION is set here, so the region comes
# from the placement read and the stand-in's region is us-east-1.
assert_header "the publish carries an AWS4-HMAC-SHA256 Authorization header" auth AWS4-HMAC-SHA256
assert_header "the credential scope names the region signed for" auth "/us-east-1/monitoring/aws4_request"
assert_token "the session token sent is the one IMDS issued" "$CANARY_TOKEN"

# Several metrics ride in one PutMetricData call as member 1, member 2, ...; the
# alarm reads a datapoint per period, not one request per metric.
expect "two metrics ride in one request" ok one "" DiskCheck=1 DiskFreeMB=14200
assert_field "the first metric is member 1" MetricData.member.1.MetricName DiskCheck
assert_field "the second metric is member 2" MetricData.member.2.MetricName DiskFreeMB
assert_field "the second value is carried" MetricData.member.2.Value 14200

# --- the environment the contract fixes --------------------------------------
OTTER_METRIC_HOST=web-2
export OTTER_METRIC_HOST
expect "OTTER_METRIC_HOST sets the Host dimension" ok one "" Heartbeat=1
assert_field "the dimension carries the configured host" MetricData.member.1.Dimensions.member.1.Value web-2
unset OTTER_METRIC_HOST

expect "the region falls back to IMDS placement" ok one "" Heartbeat=1
assert_imds "the region lookup happened" "GET /latest/meta-data/placement/region"
assert_header "the region IMDS named is the region signed for" auth "/us-east-1/monitoring/aws4_request"

OTTER_METRIC_REGION=eu-west-1
export OTTER_METRIC_REGION
expect "OTTER_METRIC_REGION is used as given" ok one "" Heartbeat=1
if grep -Fq "GET /latest/meta-data/placement/region" "$imds_log"; then
	fail "OTTER_METRIC_REGION was set, but the region was still looked up in IMDS"
else
	ok "OTTER_METRIC_REGION skips the IMDS region lookup"
fi
# ...and the configured region is the one the request is signed for. This is a
# separate assertion from the default-region case above on purpose: a script
# that signed every publish for us-east-1 would satisfy that one and still be
# wrong on a host in eu-west-1.
assert_header "the configured region is the region signed for" auth "/eu-west-1/monitoring/aws4_request"
unset OTTER_METRIC_REGION

# --- the IMDSv2 sequence, step by step ---------------------------------------
# The instance enforces IMDSv2, so each of these is a host that has no usable
# credential and must therefore publish nothing.
write_mode none ok ok 200
expect "an IMDS that will not issue a token is fatal" fail none "IMDSv2 token" Heartbeat=1

write_mode ok missing ok 200
expect "an instance role with no name is fatal" fail none "empty role name" Heartbeat=1

write_mode ok ok missing 200
expect "credentials that cannot be read are fatal" fail none "credentials for role" Heartbeat=1

# --- the endpoint -------------------------------------------------------------
# A 5xx is a publish that happened and was refused: the request is on the wire,
# the script must not call that success.
write_mode ok ok ok 500
expect "a non-2xx from CloudWatch is fatal" fail one "PutMetricData" Heartbeat=1

# --- the command line is validated before IMDS --------------------------------
# A usage error must not need a credential to report, and must not sign a
# request built from a bad pair.
write_mode ok ok ok 200
expect "a metric with no value is rejected" fail none "not of the form" Heartbeat
expect "a value that is not a number is rejected" fail none "not a number" Heartbeat=x
expect "no metrics at all is a usage error" fail none "no metrics given"

# --- credentials are never echoed ---------------------------------------------
# The canaries are what the stand-in hands out, so any of them appearing in the
# script's own output is a leak. Both paths are checked: success and failure.
write_mode ok ok ok 200
expect_no_canary "a successful publish prints no credential" Heartbeat=1

write_mode ok ok ok 500
expect_no_canary "a failed publish prints no credential" Heartbeat=1

write_mode none ok ok 200
expect_no_canary "a failed IMDS read prints no credential" Heartbeat=1

# --- outcome -----------------------------------------------------------------
if [ "$failed" -ne 0 ]; then
	echo "test-otter-metric: FAILED: $failed case(s), $passed passed" >&2
	exit 1
fi

echo "test-otter-metric: all $passed cases passed"
