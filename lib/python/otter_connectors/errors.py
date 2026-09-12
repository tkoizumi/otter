"""Exception types shared across connectors."""


class ConnectorError(Exception):
    """Base class for every error a connector raises.

    Otter treats any exception as a failed run, so the distinction that matters
    here is not *whether* an error is raised, but whether retrying could help.
    """


class ConfigError(ConnectorError):
    """The integration is misconfigured; retrying will not help.

    Raised for missing environment variables, unusable credentials and similar
    operator mistakes, so the failure is reported with a clear message instead
    of looking like a transient outage.
    """
