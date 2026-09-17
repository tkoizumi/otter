"""Every knob this integration reads, in one place.

Read as a group, before anything is built: a missing required value or a
non-numeric one fails here rather than half way through a sync. ``main`` reads
no environment variables itself.

Constructing a ``Settings`` directly is how the tests avoid touching
``os.environ``. That is the point of the dataclass rather than a handful of
module constants or another function-local block in ``main``.

The defaults live here and are *stated* in ``.env.example`` and ``otter.yaml``.
The settings are the source of truth; those files describe them.
"""

from dataclasses import dataclass
from datetime import datetime

from otter_connectors.config import env, env_bool, env_int, require_env
from otter_connectors.timeutil import parse_iso

__all__ = ["Settings"]


@dataclass(frozen=True)
class Settings:
    """What one run is configured to do.

    Grouped by what the values are *for* rather than by which module consumes
    them, so the four target settings read as one decision and the window
    settings as another.
    """

    # -- what we sync, and where it lands ---------------------------------- #
    shopify_store: str
    salesforce_instance_url: str
    salesforce_object: str
    external_id_field: str

    # -- the window one run drains ------------------------------------------ #
    page_size: int
    max_pages: int
    overlap_seconds: int
    budget_seconds: int
    backfill_from: datetime | None
    backfill_days: int

    # -- mode --------------------------------------------------------------- #
    dry_run: bool

    @classmethod
    def load(cls):
        """Read every setting from the environment, failing fast on a bad one.

        ``SALESFORCE_BATCH_SIZE`` is deliberately absent: it configures the
        Salesforce client rather than this sync, so ``salesforce_client`` reads
        it. Nothing else in the run uses it.
        """
        return cls(
            shopify_store=require_env("SHOPIFY_STORE"),
            salesforce_instance_url=require_env("SALESFORCE_INSTANCE_URL"),
            salesforce_object=env("SALESFORCE_OBJECT", "Product2"),
            external_id_field=env("SALESFORCE_EXTERNAL_ID_FIELD", "Shopify_Variant_Id__c"),
            page_size=env_int("PAGE_SIZE", 100),
            max_pages=env_int("MAX_PAGES_PER_RUN", 20),
            overlap_seconds=env_int("OVERLAP_SECONDS", 600),
            budget_seconds=env_int("RUN_BUDGET_SECONDS", 240),
            backfill_from=parse_iso(env("BACKFILL_FROM")),
            backfill_days=env_int("BACKFILL_DAYS", 30),
            dry_run=env_bool("DRY_RUN", False),
        )
