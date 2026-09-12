"""HTTP plumbing shared by the connectors.

The only subtle part is TLS. The python.org macOS installers ship no CA store:
``ssl.get_default_verify_paths().openssl_cafile`` points at a ``cert.pem`` that
was never created, so every HTTPS request fails with
``CERTIFICATE_VERIFY_FAILED`` until the operator runs Apple's
"Install Certificates.command" (which needs sudo). ``certifi`` is already
installed alongside that interpreter, so falling back to its bundle makes
integrations work without it.
"""

import os
import ssl
import urllib.request

__all__ = ["http_open", "opener", "ssl_context"]

_OPENER = None


def ssl_context():
    """An SSL context with a CA bundle that actually exists."""
    paths = ssl.get_default_verify_paths()
    has_default = (
        (paths.cafile and os.path.exists(paths.cafile))
        or (paths.capath and os.path.isdir(paths.capath))
    )
    if has_default:
        return ssl.create_default_context()

    try:
        import certifi
    except ImportError:
        # Nothing better to offer: let the interpreter report its own error.
        return ssl.create_default_context()
    return ssl.create_default_context(cafile=certifi.where())


def opener():
    """A pooled opener built once with :func:`ssl_context`."""
    global _OPENER
    if _OPENER is None:
        _OPENER = urllib.request.build_opener(
            urllib.request.HTTPSHandler(context=ssl_context())
        )
    return _OPENER


def http_open(request, timeout):
    """Open a request using the CA-aware opener.

    Raises the usual ``urllib.error.HTTPError`` / ``URLError``; the HTTPError
    carries a readable body, which callers rely on for error messages.
    """
    return opener().open(request, timeout=timeout)
