"""A small Salesforce REST client.

Covers what a sync needs from Salesforce, and nothing else:

* **Authentication.** OAuth 2.0 with the client credentials grant (the right
  choice for an unattended server: no password, no security token) or the
  username-password grant as a fallback.
* **Upsert by External ID**, batched through sObject Collections with a
  per-record fallback, so one bad record cannot stall a page.
* **Picklist awareness.** Orgs with State/Country picklists reject country and
  state values that are not on their own list, so the valid integration values
  can be read up front and matched.

It knows nothing about Shopify, or about whichever object you are writing; pass
``sobject`` and the external ID field name.
"""

import json
import time
import urllib.error
import urllib.parse
import urllib.request

from .errors import ConfigError, ConnectorError
from .http import http_open

__all__ = [
    "ADDRESS_PICKLIST_FIELDS",
    "DEFAULT_API_VERSION",
    "SalesforceClient",
    "SalesforceError",
    "SalesforceRecordError",
    "is_address_picklist_error",
    "pick_allowed",
]

DEFAULT_API_VERSION = "62.0"
DEFAULT_TIMEOUT = 60

#: Address fields a State/Country picklist may refuse.
ADDRESS_PICKLIST_FIELDS = (
    "MailingCountry", "MailingState", "MailingCountryCode", "MailingStateCode",
)

#: Errors worth retrying on the same record.
TRANSIENT_SALESFORCE_CODES = {
    "UNABLE_TO_LOCK_ROW",
    "REQUEST_LIMIT_EXCEEDED",
    "SERVER_UNAVAILABLE",
    "TIMEOUT",
}


class SalesforceError(ConnectorError):
    """Salesforce could not serve the request."""


class SalesforceRecordError(ConnectorError):
    """Salesforce rejected one record (as opposed to the whole request)."""


class SalesforceClient:
    """Talks to one org's REST API."""

    def __init__(self, instance_url, api_version=DEFAULT_API_VERSION, auth="client_credentials",
                 client_id=None, client_secret=None, username=None, password=None,
                 batch_size=200, max_attempts=4, timeout=DEFAULT_TIMEOUT):
        self.instance_url = instance_url.rstrip("/")
        self.api_version = api_version
        self.auth = auth
        self.client_id = client_id
        self.client_secret = client_secret
        self.username = username
        self.password = password
        self.batch_size = max(1, batch_size)
        self.max_attempts = max_attempts
        self.timeout = timeout

        self._access_token = None
        self._picklists = {}
        # Counted when a record is written without its mailing address because
        # the org's State/Country picklist rejected the value.
        self.address_fallbacks = 0
        self.address_fallback_reason = ""

    # -- authentication ---------------------------------------------------- #

    def _authenticate(self):
        fields = {"client_id": self.client_id, "client_secret": self.client_secret}
        if self.auth == "password":
            if not self.username or not self.password:
                raise ConfigError(
                    "SALESFORCE_AUTH=password requires SALESFORCE_USERNAME and SALESFORCE_PASSWORD")
            fields.update(
                grant_type="password",
                username=self.username,
                password=self.password,  # password + security token
            )
        elif self.auth == "client_credentials":
            fields["grant_type"] = "client_credentials"
        else:
            raise ConfigError(
                "SALESFORCE_AUTH must be client_credentials or password, got %r" % self.auth)

        request = urllib.request.Request(
            self.instance_url + "/services/oauth2/token",
            data=urllib.parse.urlencode(fields).encode("utf-8"),
            method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded",
                     "Accept": "application/json"},
        )
        try:
            with http_open(request, self.timeout) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            raise ConfigError(
                "Salesforce authentication failed (HTTP %d): %s" % (exc.code, exc.read()[:400]))
        except urllib.error.URLError as exc:
            raise SalesforceError("cannot reach Salesforce: %s" % exc.reason)

        token = payload.get("access_token")
        if not token:
            raise ConfigError("Salesforce authentication returned no access token: %s" % payload)
        self._access_token = token
        # The response carries the API host; honour it in case My Domain differs
        # from what was configured.
        if payload.get("instance_url"):
            self.instance_url = payload["instance_url"].rstrip("/")
        return token

    def access_token(self, force=False):
        """A valid bearer token, obtained on first use and cached for the run."""
        if force or not self._access_token:
            return self._authenticate()
        return self._access_token

    # -- requests ---------------------------------------------------------- #

    def _request(self, method, path, payload=None, allow_refresh=True):
        body = json.dumps(payload).encode("utf-8") if payload is not None else None

        for attempt in range(1, self.max_attempts + 1):
            request = urllib.request.Request(
                self.instance_url + path,
                data=body,
                method=method,
                headers={
                    "Authorization": "Bearer " + self.access_token(),
                    "Content-Type": "application/json",
                    "Accept": "application/json",
                },
            )
            try:
                with http_open(request, self.timeout) as response:
                    raw = response.read()
                    return response.status, (json.loads(raw) if raw else None)
            except urllib.error.HTTPError as exc:
                status = exc.code
                raw = exc.read()
            except urllib.error.URLError as exc:
                if attempt == self.max_attempts:
                    raise SalesforceError("cannot reach Salesforce: %s" % exc.reason)
                time.sleep(min(2 ** attempt, 15))
                continue

            if status == 401 and allow_refresh:
                self.access_token(force=True)
                return self._request(method, path, payload, allow_refresh=False)
            if status == 429 or status >= 500:
                if attempt == self.max_attempts:
                    raise SalesforceError(
                        "Salesforce returned HTTP %d after %d attempts" % (status, attempt))
                time.sleep(min(2 ** attempt, 15))
                continue

            detail = salesforce_message(raw)
            if status == 400 and is_transient_code(raw):
                if attempt == self.max_attempts:
                    raise SalesforceRecordError(detail)
                time.sleep(min(2 ** attempt, 10))
                continue
            if status < 300:
                return status, None
            raise SalesforceRecordError("HTTP %d: %s" % (status, detail))

        raise SalesforceError("Salesforce request failed after %d attempts" % self.max_attempts)

    def describe(self, sobject):
        """The org's own description of an object: every field, its type,
        length and flags.

        Raises rather than returning a default, unlike :meth:`picklist_values`:
        a caller asking what an object looks like needs to know when the answer
        is "I could not find out".
        """
        _, described = self._request(
            "GET", "/services/data/v%s/sobjects/%s/describe" % (self.api_version, sobject))
        if not isinstance(described, dict) or "fields" not in described:
            raise SalesforceError(
                "Salesforce returned no describe for %s: %r" % (sobject, str(described)[:200]))
        return described

    def picklist_values(self, sobject, field):
        """The org's valid integration values for a picklist field, lowercased.

        Returns ``None`` when the field is not a picklist, meaning "send
        whatever you have". Describing once per run lets callers match the org's
        own values instead of assuming ISO codes.
        """
        cache_key = (sobject, field)
        if cache_key in self._picklists:
            return self._picklists[cache_key]

        values = None
        try:
            for entry in self.describe(sobject).get("fields") or []:
                if entry.get("name") != field:
                    continue
                entries = entry.get("picklistValues") or []
                if entries:
                    values = {str(v.get("value", "")).strip().lower()
                              for v in entries if v.get("active", True)}
                break
        except (SalesforceError, SalesforceRecordError):
            values = None

        self._picklists[cache_key] = values
        return values

    # -- upsert ------------------------------------------------------------ #

    def upsert(self, sobject, external_id_field, records):
        """Upsert records by external ID. Returns ``(written, failures)``.

        ``failures`` is a list of ``(external_id, message)``. Permanent
        per-record failures are returned rather than raised so one bad record
        cannot stall every later one.
        """
        written = 0
        failures = []

        for chunk in chunks(records, self.batch_size):
            if len(chunk) == 1:
                ok, message = self._upsert_one(sobject, external_id_field, chunk[0])
                if ok:
                    written += 1
                else:
                    failures.append((chunk[0][external_id_field], message))
                continue

            # sObject Collections upsert:
            #   PATCH /composite/sobjects/{SObject}/{ExternalIdField}
            #   ?allOrNone=false          <- a query parameter, not a body field
            #   {"records": [ {..., "attributes": {"type": SObject}} ]}
            # and the external ID field IS carried in each record here, unlike
            # the single-record form below where it comes from the URL.
            path = "/services/data/v%s/composite/sobjects/%s/%s?allOrNone=false" % (
                self.api_version, sobject, external_id_field,
            )
            payload = {
                "records": [dict(record, attributes={"type": sobject}) for record in chunk],
            }
            try:
                _, results = self._request("PATCH", path, payload)
            except SalesforceError:
                raise
            except SalesforceRecordError as exc:
                # The batch as a whole was rejected; fall back to one at a time
                # so the rest of the page still makes it across. Keep the batch
                # reason too -- reporting only the retry's error can hide the
                # real cause.
                batch_error = str(exc)
                for record in chunk:
                    ok, message = self._upsert_one(sobject, external_id_field, record)
                    if ok:
                        written += 1
                    else:
                        failures.append((record[external_id_field],
                                         combine_errors(message, batch_error)))
                continue

            if not isinstance(results, list):
                raise SalesforceError("unexpected composite response: %r" % (results,))

            for record, result in zip(chunk, results):
                if result.get("success"):
                    written += 1
                    continue
                message = "; ".join(
                    (error or {}).get("message", "") for error in (result.get("errors") or [])
                ) or "unknown Salesforce error"
                ok, retry_message = self._upsert_one(sobject, external_id_field, record)
                if ok:
                    written += 1
                elif retry_message and retry_message != message:
                    failures.append((record[external_id_field],
                                     combine_errors(retry_message, message)))
                else:
                    failures.append((record[external_id_field], retry_message or message))

        return written, failures

    def _upsert_one(self, sobject, external_id_field, record):
        external_id = record[external_id_field]
        path = "/services/data/v%s/sobjects/%s/%s/%s" % (
            self.api_version, sobject, external_id_field, urllib.parse.quote(str(external_id)),
        )
        # The external ID is carried in the URL, and Salesforce rejects the
        # request outright if it also appears in the body:
        #   "The <field> field should not be specified in the sobject data."
        body = {key: value for key, value in record.items() if key != external_id_field}
        try:
            self._request("PATCH", path, body)
            return True, ""
        except SalesforceRecordError as exc:
            message = str(exc)
            if not is_address_picklist_error(message):
                return False, message
            # The org's State/Country picklist does not accept the code we sent.
            # Losing the address is far better than losing the record, so retry
            # without it and report the reason once per run.
            trimmed = {k: v for k, v in body.items() if k not in ADDRESS_PICKLIST_FIELDS}
            if len(trimmed) == len(body):
                return False, message
            try:
                self._request("PATCH", path, trimmed)
            except SalesforceRecordError as second:
                return False, str(second)
            self.address_fallbacks += 1
            if not self.address_fallback_reason:
                self.address_fallback_reason = message
            return True, ""


# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #


def pick_allowed(primary, fallback, allowed):
    """Choose a value the org's picklist accepts, or ``""`` to omit the field.

    ``allowed`` is the org's set of integration values (lowercased), or ``None``
    when the field is free text. Callers that have both an ISO code and a full
    name can offer both, so whichever the org uses is matched without guessing.
    """
    if allowed is None:
        return (primary or fallback or "").strip()
    for candidate in (primary, fallback):
        candidate = (candidate or "").strip()
        if candidate and candidate.lower() in allowed:
            return candidate
    return ""


def is_address_picklist_error(message):
    """Whether Salesforce refused the record over the mailing country/state.

    Covers both a restricted-picklist rejection and the free-text wording
    ("...select a country/territory from the list of valid countries.").
    """
    text = message or ""
    looks_like_error = ("INVALID_OR_NULL_FOR_RESTRICTED_PICKLIST" in text
                        or "country/territory from the list" in text
                        or "problem with this country" in text)
    if not looks_like_error:
        return False
    lowered = text.lower()
    return "mailing" in lowered or "country" in lowered or "state" in lowered


def combine_errors(single, batch):
    """Report the per-record error, keeping the batch error when it differs."""
    single = (single or "").strip()
    batch = (batch or "").strip()
    if not single:
        return batch
    if not batch or batch in single:
        return single
    return "%s (batch: %s)" % (single, batch)


def salesforce_message(raw):
    """Render a Salesforce error body as a single readable line."""
    try:
        payload = json.loads(raw)
    except ValueError:
        return raw[:400].decode("utf-8", "replace")

    if isinstance(payload, list) and payload:
        payload = payload[0]
    if isinstance(payload, dict):
        message = payload.get("message") or json.dumps(payload)[:400]
        code = payload.get("errorCode") or payload.get("statusCode")
        return "%s: %s" % (code, message) if code else message
    return str(payload)[:400]


def is_transient_code(raw):
    """Whether a Salesforce error body carries a retryable error code."""
    try:
        payload = json.loads(raw)
    except ValueError:
        return False
    if isinstance(payload, list) and payload:
        payload = payload[0]
    return isinstance(payload, dict) and payload.get("errorCode") in TRANSIENT_SALESFORCE_CODES


def chunks(items, size):
    """Yield ``items`` in lists of at most ``size``."""
    for start in range(0, len(items), size):
        yield items[start:start + size]
