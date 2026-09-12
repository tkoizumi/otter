"""Timestamps in the one format these connectors speak.

Everything crossing an API boundary is an ISO 8601 UTC string, and Otter state
stores the same thing, so a single pair of helpers avoids a dozen ad-hoc
``strftime`` calls.
"""

from datetime import datetime, timedelta, timezone

__all__ = ["parse_iso", "to_iso", "utcnow"]

#: What Shopify's ``updated_at:>'...'`` search filter and Salesforce both accept.
ISO_FORMAT = "%Y-%m-%dT%H:%M:%SZ"


def utcnow():
    """The current time, timezone-aware, in UTC."""
    return datetime.now(timezone.utc)


def to_iso(moment):
    """Render a datetime as an ISO 8601 UTC string."""
    return moment.astimezone(timezone.utc).strftime(ISO_FORMAT)


def parse_iso(text):
    """Parse an ISO 8601 string, returning ``None`` when it is not one.

    Tolerating junk matters more than being strict: this reads persisted state
    written by earlier runs, and a bad value should reopen a window rather than
    crash the integration.
    """
    if not text:
        return None
    if not isinstance(text, str):
        return None
    try:
        return datetime.fromisoformat(text.replace("Z", "+00:00")).astimezone(timezone.utc)
    except ValueError:
        return None


def days_ago(moment, days):
    """``moment`` shifted back by whole days."""
    return moment - timedelta(days=days)
