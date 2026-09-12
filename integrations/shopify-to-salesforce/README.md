# shopify-to-salesforce

Upserts Shopify customers into Salesforce every five minutes, incrementally and
idempotently.

```
Shopify Admin API (GraphQL)          Otter                      Salesforce REST API
  customers(updated_at:>watermark)  ──►  child Python process  ──►  composite/sobjects upsert
  cursor pagination                     state: watermark,           by Shopify_Customer_Id__c
                                        page cursor, dead letters
```

Runtime primitives (scheduling, the watermark, retries, timeouts, logs, run
history) are all Otter's. The vendor clients are not — they live in a shared
library, so this file holds only what is specific to this sync.

## Code layout

```text
lib/python/otter_connectors/          shared, reusable, no third-party deps
├── shopify.py        Shopify GraphQL client: client credentials grant,
│                     throttle backoff, Relay cursor paging
├── salesforce.py     Salesforce REST client: OAuth, batched upsert by
│                     External ID, picklist-aware values, address fallback
├── records.py        record building: dotted paths, join, drop empties,
│                     truncate, mapping validation
├── checkpoint.py     Watermark: resumable "what have I processed?" state
├── config.py         env / env_int / env_bool / require_env
├── http.py           the CA-aware opener both clients share
├── timeutil.py       utcnow / to_iso / parse_iso
└── errors.py         ConnectorError, ConfigError

integrations/shopify-to-salesforce/   this integration only
├── otter.yaml        when and how it runs
├── source.py         what we read from Shopify: the query and its paging
├── mapping.py        where it lands: the Contact mapping and field lengths
├── main.py           orchestration: clients, watermark, page loop, retries
└── tests/            mapping and query, testable without a daemon
```

**Editing the integration usually means editing `source.py` and `mapping.py`
only.** Adding a field is a line in each (the query, and the mapping) plus a
length in `MAX_FIELD_LENGTH`; `main.py` should not need touching, and
`tests/test_source.py` fails if the query and the mapping drift apart.

`mapping.py` is deliberately pure — no environment reads, no I/O, nothing
imported from `main` or `source` — so it is testable on a plain dict and could
be lifted into a shared package unchanged.

Nothing special makes the sibling imports work: Otter runs `python3 main.py`
with the working directory set to the integration directory, so Python puts
that directory on `sys.path` itself.

The manifest points at the shared library, which Otter puts on the child's
`PYTHONPATH` — no install step, and `otter validate` fails if the path is wrong:

```yaml
python:
  executable: python3
  path:
    - ../../lib/python
```

See [`lib/python/README.md`](../../lib/python/README.md) for why this is not part
of the Otter SDK, and how to reuse it from another integration.

- [Before you start](#before-you-start)
- [Install](#install)
- [Configuration](#configuration)
- [First run](#first-run)
- [Turn on the schedule](#turn-on-the-schedule)
- [Day two operations](#day-two-operations)
- [How it behaves when things go wrong](#how-it-behaves-when-things-go-wrong)
- [Field mapping](#field-mapping)
- [Gotchas](#gotchas)
- [Later improvements](#later-improvements)

---

## Before you start

Three things must exist before the first run. None of them are things Otter can
do for you.

### 1. A Shopify app in the Dev Dashboard

Two things have changed in Shopify's auth story, and both matter here:

- **The Admin REST API is a legacy API** ([shopify.dev](https://shopify.dev/docs/api/admin-rest/2025-10/resources/customer)),
  so this integration uses the GraphQL Admin API.
- **Admin-created custom apps can no longer be created.** There is no longer a
  token to copy out of the Shopify admin. A server-side integration acting on
  your own stores uses the
  [client credentials grant](https://shopify.dev/docs/apps/build/authentication-authorization/client-credentials-grant):
  it exchanges its own client ID and secret for an access token that is valid
  for **24 hours**, and renews it by repeating the same request. This
  integration does that automatically at the start of every run, so there is no
  long-lived Shopify token anywhere.

Set the app up:

1. Create an app in the **[Dev Dashboard](https://dev.shopify.com/dashboard/)**
   (not the Shopify admin's *Develop apps* page).
2. On the app's **version**, select the `read_customers` scope and release it.
   Scopes live on the version, so adding one later means releasing a new version
   and approving the change on the store.
3. **Install the app on your store.**
4. Copy the **Client ID** and **Client secret** from the app's **Settings**.
   These are what go in `SHOPIFY_CLIENT_ID` / `SHOPIFY_CLIENT_SECRET`.

**The catch that stops most people:** the client credentials grant only works
when the app and the store belong to the **same Shopify organization**. Having
the app installed is not enough — the store must appear under **Dev stores** in
that organization in the Dev Dashboard. A store belonging to a client, or a dev
store created from the Shopify admin rather than the Dev Dashboard, is not in
your organization, and every token request fails with:

```text
Oauth error shop_not_permitted: Client credentials cannot be performed on this shop.
```

If your store is not in your organization, client credentials cannot reach it.
Distribute the app with **custom distribution** so a merchant installs it, and
switch to the authorization code grant (which needs a redirect flow and a stored
refresh token — a different design from this one).

5. **Request protected customer data access.** Name, email, phone and address
   are protected customer data. Without approval for your app, Shopify returns
   those fields as `null` and the sync silently creates Contacts with only a
   last name. This is the single most common cause of an "empty" sync that
   reports success.

Then confirm the grant and the query work before wiring anything up:

```bash
export SHOPIFY_STORE=your-store.myshopify.com
export SHOPIFY_CLIENT_ID=...
export SHOPIFY_CLIENT_SECRET=...

# 1. Exchange credentials for a 24 hour token
TOKEN=$(curl -sS -X POST "https://$SHOPIFY_STORE/admin/oauth/access_token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d grant_type=client_credentials \
  -d client_id="$SHOPIFY_CLIENT_ID" \
  -d client_secret="$SHOPIFY_CLIENT_SECRET" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("access_token") or d)')
echo "$TOKEN"   # 32 hex chars, plus scope and expires_in=86399 in the response

# 2. Use it, and confirm protected customer data is approved
curl -sS -X POST "https://$SHOPIFY_STORE/admin/api/2026-07/graphql.json" \
  -H "X-Shopify-Access-Token: $TOKEN" \
  -H 'Content-Type: application/json' \
  --data @- <<'JSON'
{"query":"query { customers(first: 2, sortKey: UPDATED_AT) { nodes { id email firstName lastName phone updatedAt } } }"}
JSON
```

You should see real names and emails. If they are `null`, fix the protected-data
approval first. If GraphQL rejects `sortKey` alongside `query` on your API
version, set `SHOPIFY_SORT_KEY` (see [Configuration](#configuration)).

### 2. A Salesforce External ID field

The upsert keys on a custom field that must be marked **External ID** and
**Unique**:

1. Setup → Object Manager → **Contact** → Fields & Relationships → **New**
2. Type **Text**, length **255**
3. Field Label `Shopify Customer Id`, Field Name `Shopify_Customer_Id__c`
4. Tick **External ID** and **Unique**, then Save

If the field is not marked External ID, every write fails with
`INVALID_FIELD`. Rename it and update `SALESFORCE_EXTERNAL_ID_FIELD` if you
prefer a different name.

### 3. Salesforce OAuth credentials

This integration defaults to the **client credentials** flow, which is the
right choice for an unattended server: no password, no security token, nothing
to rotate monthly.

1. Setup → App Manager → **New Connected App**
2. Enable OAuth Settings. Any callback URL will do
   (`https://login.salesforce.com/services/oauth2/success`).
3. OAuth scope: **Manage user data via APIs (`api`)**
4. Tick **Enable Client Credentials Flow**, then set the **Run As** user to a
   dedicated integration user that can read/write Contacts.
5. Save, wait a few minutes for it to propagate, then copy the **Consumer Key**
   and **Consumer Secret**.
6. Make sure **My Domain** is deployed — the token endpoint is your My Domain
   URL, not `login.salesforce.com`.

Prefer the **username-password** flow instead? Set `SALESFORCE_AUTH: password`
and provide `SALESFORCE_USERNAME` plus `SALESFORCE_PASSWORD` (the password with
the security token appended). It works, but it needs the connected app's
"Allow OAuth Username-Password Flows" enabled and it breaks whenever the user
changes their password or the org's IP restrictions change.

---

## Install

Any directory containing an `otter.yaml` is an integration. Put this one
wherever your deployment keeps integrations:

```bash
sudo mkdir -p /srv/otter/integrations
sudo cp -r integrations/shopify-to-salesforce /srv/otter/integrations/
sudo chown -R otter:otter /srv/otter
```

Secrets go in the **daemon's** environment, never in the manifest. Otter reads
the keys listed under `secrets:` from its own environment and injects them into
the child process, and it refuses to start Python at all if one is missing.
`otter inspect` prints `env:` values, so anything in the manifest is public —
only put non-secret settings there.

```bash
sudo install -m 600 -o otter -g otter /dev/null /etc/otter/shopify-to-salesforce.env
sudo tee /etc/otter/shopify-to-salesforce.env >/dev/null <<'EOF'
SHOPIFY_CLIENT_ID=...
SHOPIFY_CLIENT_SECRET=...
SALESFORCE_CLIENT_ID=3MVG9...
SALESFORCE_CLIENT_SECRET=...
EOF
```

See [`.env.example`](.env.example).

Validate the manifest before going further — this runs locally and needs no
daemon:

```bash
./bin/otter validate /srv/otter/integrations/shopify-to-salesforce
# ok: shopify-to-salesforce (/srv/otter/integrations/shopify-to-salesforce/otter.yaml)
```

---

## Configuration

### In `otter.yaml` (non-secret, edits go here)

| Key | Default in the manifest | Notes |
| --- | --- | --- |
| `SHOPIFY_STORE` | `your-store.myshopify.com` | Your `*.myshopify.com` domain. |
| `SHOPIFY_API_VERSION` | `2026-07` | Pin it; bump deliberately. |
| `SALESFORCE_INSTANCE_URL` | `https://your-domain.my.salesforce.com` | My Domain; also the token endpoint. |
| `SALESFORCE_API_VERSION` | `62.0` | Any version your org supports. |
| `SALESFORCE_AUTH` | `client_credentials` | Or `password`. |
| `SALESFORCE_OBJECT` | `Contact` | `Account` works but needs a different mapping. |
| `SALESFORCE_EXTERNAL_ID_FIELD` | `Shopify_Customer_Id__c` | Must be External ID + Unique. |
| `BACKFILL_FROM` | `2026-01-01T00:00:00Z` | Where the *first ever* run starts. |

### In the daemon's environment (operational knobs, set per deployment)

These are read with sensible defaults and are deliberately **not** in the
manifest, so exporting them for the daemon overrides them without editing a
committed file. Remember the manifest wins for any key it also defines.

| Key | Default | Purpose |
| --- | --- | --- |
| `PAGE_SIZE` | `100` | Customers per Shopify page. |
| `MAX_PAGES_PER_RUN` | `20` | Upper bound on work per run (20 × 100 = 2000 customers). |
| `RUN_BUDGET_SECONDS` | `240` | Wall-clock budget, kept under the 300s timeout. |
| `OVERLAP_SECONDS` | `600` | How far back each window re-scans. |
| `SALESFORCE_BATCH_SIZE` | `200` | Records per composite call; `1` forces per-record writes. |
| `SYNC_ADDRESS` | `1` | Set `0` to skip mailing address fields. |
| `DRY_RUN` | `0` | Set `1` to read Shopify and log writes without touching Salesforce. |
| `SHOPIFY_SORT_KEY` | `UPDATED_AT` | Change if your API version rejects it. |
| `SHOPIFY_API_BASE` | derived from store + version | Override to point at a mock. |
| `SHOPIFY_TOKEN_URL` | `https://<store>/admin/oauth/access_token` | Override to point at a mock. |
| `SALESFORCE_USERNAME` / `SALESFORCE_PASSWORD` | unset | Password flow only. |

### Secrets (daemon environment, listed under `secrets:`)

`SHOPIFY_CLIENT_ID`, `SHOPIFY_CLIENT_SECRET`, `SALESFORCE_CLIENT_ID`,
`SALESFORCE_CLIENT_SECRET`.

`SHOPIFY_ACCESS_TOKEN` is read from the environment but deliberately not listed,
so it stays optional: set it only if you hold a pre-generated token from a
legacy admin-created custom app, and it is then used verbatim instead of the
grant.

`SALESFORCE_USERNAME` and `SALESFORCE_PASSWORD` are likewise optional, for the
password flow. If you use either pair, add the keys to `secrets:` too — then a
missing value fails the run immediately with a clear message instead of part
way through.

### Why there is no Shopify token in this file

Tokens from the client credentials grant last 24 hours, and Otter runs each
attempt in a **fresh process**, so an in-memory cache would never survive a run
anyway. The integration therefore simply requests a token at the start of every
run: about 288 extra requests a day on a 5-minute schedule, nothing persisted,
and no stale-token failure mode. It refreshes once mid-run if Shopify answers
`401`/`403`, which covers a secret rotation or a token lapsing between pages.

If you would rather not make that call every 5 minutes, cache
`access_token` + `expires_in` in Otter state and reuse it until it is close to
expiry. It is a small change to `ShopifyClient.access_token()` — but you then
own the staleness problem, which is why it is not the default.

---

## First run

Start the daemon in dry-run mode. It will log exactly what it *would* write and
change nothing — not the watermark, not Salesforce.

```bash
set -a; . /etc/otter/shopify-to-salesforce.env; set +a
DRY_RUN=1 ./bin/otterd --integrations /srv/otter/integrations --data /var/lib/otter
```

In another shell:

```bash
./bin/otter integrations
./bin/otter run shopify-to-salesforce
./bin/otter logs <run-id> | head -40
```

Look for `dry run: would upsert` lines containing real names and emails. Then
check nothing was written:

```bash
./bin/otter state get shopify-to-salesforce sync_cursor
# otter: shopify-to-salesforce/sync_cursor is not set
```

Now do it for real. Restart the daemon without `DRY_RUN`:

```bash
set -a; . /etc/otter/shopify-to-salesforce.env; set +a
./bin/otterd --integrations /srv/otter/integrations --data /var/lib/otter
./bin/otter run shopify-to-salesforce
```

The first run backfills from `BACKFILL_FROM`, `MAX_PAGES_PER_RUN` pages at a
time. If you have more customers than fit in one run it will exit
**succeeded** with `complete: false`, and the next run continues from the saved
page cursor — no data is lost and nothing is written twice. Watch progress:

```bash
./bin/otter state get shopify-to-salesforce last_run
./bin/otter state get shopify-to-salesforce in_progress_cursor
```

Once a window drains, the watermark advances and later runs only pick up
customers changed since. Confirm with a second run — it should fetch nothing:

```bash
./bin/otter logs "$(./bin/otter run shopify-to-salesforce)" | grep 'sync finished'
# sync finished {"complete":true,"failed":0,"fetched":0,"pages":1,"written":0,...}
```

---

## Turn on the schedule

The manifest already has the trigger:

```yaml
trigger:
  cron: "*/5 * * * *"
```

Otter registers cron triggers from the manifest on every start, so there is
nothing else to enable — a daemon running with this integration will fire every
five minutes. To test without the schedule first, simply comment the `trigger:`
block out while you are doing the first runs above; manual runs work either way.

A systemd unit:

```ini
[Unit]
Description=Otter integration runtime
After=network-online.target

[Service]
User=otter
EnvironmentFile=/etc/otter/shopify-to-salesforce.env
ExecStart=/usr/local/bin/otterd \
  --integrations /srv/otter/integrations \
  --data /var/lib/otter \
  --workers 4 \
  --log-format json
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

---

## Day two operations

```bash
otter status                                   # queue depth, run counts
otter inspect shopify-to-salesforce            # config, cron, next fire time
otter runs --integration shopify-to-salesforce --limit 20
otter logs <run-id> --follow
otter state get shopify-to-salesforce last_run
otter state get shopify-to-salesforce failed_customers
otter state get shopify-to-salesforce failed_total
```

**Re-read the dead letters.** Records Salesforce permanently rejected are kept
in `failed_customers` (most recent 100) with the exact error; `failed_total`
counts all of them. Fix the cause — usually a State/Country picklist — then
re-sync those customers by rewinding the watermark:

```bash
otter state set shopify-to-salesforce sync_cursor '"2026-01-01T00:00:00Z"'
otter state delete shopify-to-salesforce in_progress_cursor
otter state delete shopify-to-salesforce in_progress_window_start
otter run shopify-to-salesforce
```

Rewinding is safe: every write is an upsert, so re-syncing updates the same
Contacts rather than duplicating them.

**Force a full re-sync.** Same as above with `BACKFILL_FROM`'s value.

**Pause the integration.** Comment out the `trigger:` block and restart the
daemon; manual runs still work. Queued and running jobs are unaffected.

---

## How it behaves when things go wrong

| Situation | What happens | Why |
| --- | --- | --- |
| Shopify 5xx or unreachable | Retried in-process, then the run fails and Otter retries with backoff | Infrastructure blip; the run is safe to repeat |
| Shopify throttles (GraphQL `THROTTLED`) | Sleeps until the leaky bucket can afford the next query | Avoids burning the retry policy on rate limits |
| Access token lapses or is revoked mid-run | Refreshed once via the client credentials grant, then the page is retried | 24 hour tokens, and a secret rotation invalidates them early |
| Credentials wrong, or store not in the app's organization | Run fails immediately with `shop_not_permitted` or the raw Shopify error | Configuration error; retries cannot help |
| Missing `read_customers` scope or protected-data approval | Request succeeds but names/emails come back `null` | Shopify returns what the scopes allow, so this looks like success |
| One customer rejected by Salesforce (`INVALID_FIELD`, bad picklist) | Recorded in `failed_customers`, run still succeeds, watermark advances | One malformed customer must not stall the other 2000 |
| Row locked / request limit (`UNABLE_TO_LOCK_ROW`, 429) | Retried, then the single record is retried on its own | Transient; the batch should not be lost |
| Run hits the time budget | Exits **succeeded** with the page cursor saved | Better than being killed by the timeout and marked `timed_out` |
| Process killed / daemon restarted mid-window | Next run resumes from the saved page cursor | The watermark only moves when a window is fully drained |
| Missing secret in the daemon env | Run fails before Python starts, and is **not** retried | Caught by Otter, not by this code |

---

## Field mapping

| Shopify | Salesforce Contact |
| --- | --- |
| `id` (numeric part) | `Shopify_Customer_Id__c` (the upsert key) |
| `lastName` | `LastName` — required; falls back to `firstName`, then `Shopify Customer <id>` |
| `firstName` | `FirstName` |
| `email` | `Email` |
| `phone` | `Phone` |
| `defaultAddress.address1` + `address2` | `MailingStreet` |
| `defaultAddress.city` | `MailingCity` |
| `defaultAddress.provinceCode` | `MailingState` |
| `defaultAddress.zip` | `MailingPostalCode` |
| `defaultAddress.countryCodeV2` | `MailingCountry` |

Empty values are **omitted**, not sent as `null`, so a sync never wipes a field
someone filled in by hand — the trade-off is that clearing a phone number in
Shopify will not clear it in Salesforce.

To add a field, add a line to `contact_mapping()` — a source path, a
`(path, transform)` pair, or a callable — plus a length in `MAX_FIELD_LENGTH`
if the target field has one. The mapping is data, so it reads as the mapping
rather than as the plumbing around it:

```python
mapping = {
    external_id_field: contact_external_id,                       # logic
    "LastName": contact_last_name,                                # logic
    "FirstName": contact_first_name,                              # logic
    "Email": "email",                                            # source path
    "MailingStreet": joined("defaultAddress.address1",
                            "defaultAddress.address2"),
}
```

Anything that does not fit is just a Python callable, so there is no mapping
language to learn or outgrow. `integrations/shopify-to-salesforce/tests/`
pins the mapping down — it is the part that changes most often.

Anything beyond the standard fields above plus the external ID needs a matching
custom field in Salesforce first, or every write fails with `INVALID_FIELD`.

---

## Gotchas

**`CERTIFICATE_VERIFY_FAILED` on a developer Mac.** The python.org macOS
installers ship no CA store: `ssl.get_default_verify_paths().openssl_cafile`
points at a `cert.pem` that was never created, so *every* HTTPS request from
that interpreter fails — Shopify and Salesforce alike. `main.py` detects this
and falls back to `certifi`'s bundle, which is already installed alongside that
Python, so no configuration is needed. The underlying interpreter can also be
fixed properly with `sudo "/Applications/Python <ver>/Install Certificates.command"`
(worth doing if you use that Python for anything else).

**State and Country picklists.** If your org has them enabled, `MailingState`
and `MailingCountry` are restricted picklists. `provinceCode`/`countryCodeV2`
(ISO codes) are normally what they expect, but a mismatched value yields
`INVALID_OR_NULL_FOR_RESTRICTED_PICKLIST` and a dead letter. Either align the
picklist values or set `SYNC_ADDRESS=0`.

**Protected customer data.** Covered above, and worth repeating: without
approval, Shopify returns `null` for names, emails, phones and addresses and
the sync looks like it "works" while producing empty Contacts.

**Duplicates.** The upsert matches on `Shopify_Customer_Id__c` only. If the org
already has Contacts created by hand or by another tool, they will not be
matched and you will get a second Contact with the same email. Pick a matching
strategy before the first run — it is much easier than deduplicating later.

**API limits.** A 5-minute incremental sync is small, but a first backfill of
tens of thousands of customers is not: Salesforce enforces a daily API request
limit and Shopify enforces a GraphQL cost budget. Keep `MAX_PAGES_PER_RUN`
modest and let the backfill take a few hours rather than hammering both APIs.
For a one-off migration of a very large customer base, use Salesforce Bulk API
2.0 for the initial load and let this integration keep it current afterwards.

**Repeated retries and the cron.** `retry.attempts` is 3 with a 30s initial
delay, chosen so a retry lands inside the next 5-minute tick. If the sync is
broken for a long time, failed runs and cron ticks can queue up; because every
run is watermark-based and idempotent, the redundant ones are fast no-ops and
the backlog drains, but `otter status` will show queue depth while it does.

**`SHOPIFY_API_VERSION` matters.** Shopify releases quarterly and retires
versions; the manifest pins one deliberately. Bump it on your own schedule and
watch the first run after the change.

---

## Later improvements

- **Shopify webhooks.** Register `customers/create` and `customers/update`
  webhooks pointing at Otter's `POST /v1/hooks/shopify-to-salesforce` for
  near-real-time sync. You still want this polling integration as the
  reconciliation safety net, since webhooks can be dropped. It needs the daemon
  reachable from Shopify over TLS, with the generated webhook token.
- **Bulk API for the backfill**, as above.
- **Bidirectional sync.** Deliberately not attempted here. Two-way sync needs
  conflict resolution and a change-detection strategy; it is a project, not a
  config change.
- **A second destination.** If you later want the same customers in a
  warehouse too, that is a separate integration directory subscribing to the
  same Shopify data. Otter has no DAG to complicate it.
