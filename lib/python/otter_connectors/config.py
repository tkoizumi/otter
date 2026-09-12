"""Helpers for reading an integration's configuration from the environment.

The manifest's ``env`` block and Otter's secret injection both arrive as
environment variables, so every integration needs the same handful of readers.
"""

import os

from .errors import ConfigError

__all__ = ["env", "env_bool", "env_int", "require_env"]


def env(name, default=None):
    """Return a stripped environment value, or ``default`` when unset/blank."""
    value = os.environ.get(name)
    if value is None or value.strip() == "":
        return default
    return value.strip()


def env_int(name, default):
    """Return an integer environment value, or ``default`` when unset."""
    raw = env(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError:
        raise ConfigError("%s must be an integer, got %r" % (name, raw))


def env_bool(name, default=False):
    """Return a boolean environment value, or ``default`` when unset."""
    raw = env(name)
    if raw is None:
        return default
    return raw.lower() in ("1", "true", "yes", "on")


def require_env(name):
    """Return an environment value, raising ``ConfigError`` when it is missing."""
    value = env(name)
    if value is None:
        raise ConfigError("%s is not set" % name)
    return value
