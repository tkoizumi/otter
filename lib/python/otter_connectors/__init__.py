"""Shared client code for Otter integrations.

This package is deliberately *not* part of the Otter runtime SDK (``otter``).
Otter's rule is that the runtime only carries primitives almost every reliable
integration needs -- state, logs, triggers, checkpoints. A Shopify or Salesforce
client is not one of those, so it lives here instead: outside the daemon, in
plain Python you can read, fork and version independently of the runtime.

It ships with no third-party dependencies, so an integration can import it with
no install step at all. Point the manifest at it::

    python:
      path:
        - ../../lib/python
"""

__all__ = [
    "checkpoint",
    "config",
    "errors",
    "http",
    "records",
    "salesforce",
    "shopify",
    "timeutil",
]
