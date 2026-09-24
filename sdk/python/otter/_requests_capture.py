"""requests instrumentation for Otter HTTP capture.

The seam is ``requests.sessions.Session.send``. Every public way to make a
request in ``requests`` funnels through it: the module-level helpers build a
``Session`` and call ``request``, and ``Session.request`` prepares the request and
calls ``send``. Patching one method therefore covers ``requests.get``, a reused
``Session``, ``Session.request`` and ``session.send`` alike.

Two behaviours match the urllib adapter deliberately:

* **The caller's objects are untouched.** The real ``requests.Response`` is
  returned. A streamed response has its read paths observed as the application
  consumes them; a non-streamed response has already been read by ``requests``
  itself, so its buffered bytes are inspected without touching the network.
* **Exceptions pass through unchanged.** A transport failure is recorded and then
  re-raised as the very same object.

Redirects are followed by ``Session.send`` calling itself. Those inner hops
belong to the outer exchange, whose final URL already reflects them, so a
thread-local depth guard records the exchange once.
"""

import os
import threading
from typing import Any, List, Optional, Tuple

from . import _capture

__all__ = ["NAME", "install", "uninstall"]

NAME = _capture.ADAPTER_REQUESTS

_install_lock = threading.Lock()
_original_send: Any = None
#: The active capture, or None. Deliberately not named ``_capture``: that name
#: belongs to the module.
_active_capture: Optional[Any] = None
#: requests' PreparedRequest type, used to skip a caller's misuse (passing a
#: Request to send) rather than recording it as a transport failure.
_prepared_request: Any = None

#: Depth of nested ``Session.send`` calls on this thread. requests follows a
#: redirect by calling ``send`` again; recording those inner hops separately
#: would present one exchange as several.
_depth = threading.local()


def install(capture: _capture.Capture) -> bool:
    """Start capturing requests, if it is installed in this interpreter."""
    global _original_send, _active_capture, _prepared_request
    try:
        import requests
        import requests.sessions
    except BaseException:
        return False

    with _install_lock:
        _active_capture = capture
        _prepared_request = requests.PreparedRequest
        _capture.register_internal_frames(
            _package_dir(requests), _package_dir(requests.sessions)
        )
        if _original_send is None:
            _original_send = requests.sessions.Session.send
            requests.sessions.Session.send = _capturing_send
    return True


def uninstall() -> None:
    """Stop capturing and restore the original ``Session.send``."""
    global _original_send, _active_capture
    try:
        import requests.sessions
    except BaseException:
        return
    with _install_lock:
        if _original_send is not None:
            requests.sessions.Session.send = _original_send
            _original_send = None
        _active_capture = None


def _package_dir(module: Any) -> str:
    path = getattr(module, "__file__", "") or ""
    return os.path.dirname(path) if path else ""


def _capturing_send(self: Any, request: Any, **kwargs: Any) -> Any:
    original = _original_send
    capture = _active_capture
    if original is None or capture is None or _capture.is_suppressed():
        return original(self, request, **kwargs)
    # requests raises for anything that is not a PreparedRequest. That is a
    # caller bug, not network traffic, so it is left for requests to report.
    if _prepared_request is not None and not isinstance(request, _prepared_request):
        return original(self, request, **kwargs)

    depth = getattr(_depth, "value", 0)
    if depth > 0:
        return original(self, request, **kwargs)

    record = capture.begin(
        _request_method(request),
        _request_url(request),
        _request_headers(request),
        _request_body(request),
    )

    _depth.value = depth + 1
    try:
        response = original(self, request, **kwargs)
    except BaseException as exc:
        # requests raises on a transport failure; an HTTP error status is a
        # normal response and is recorded as one.
        capture.note_transport_error(record, exc)
        raise
    finally:
        _depth.value = depth

    _note_response(capture, record, response)
    _observe_body(capture, record, response)
    return response


# --------------------------------------------------------------- request side

def _request_method(request: Any) -> str:
    try:
        return str(request.method or "GET")
    except BaseException:
        return "GET"


def _request_url(request: Any) -> str:
    try:
        return str(request.url or "")
    except BaseException:
        return ""


def _request_headers(request: Any) -> List[Tuple[str, str]]:
    pairs: List[Tuple[str, str]] = []
    try:
        headers = getattr(request, "headers", None)
        if headers is not None:
            for name, value in headers.items():
                pairs.append((name, value))
    except BaseException:
        pass
    return pairs


def _request_body(request: Any) -> Any:
    """Return the prepared body without consuming a stream.

    ``PreparedRequest.body`` is bytes, a str, a file-like object or an iterable.
    The capture layer knows how to describe each of those; returning it as-is is
    what keeps a streamed upload from being read here.
    """
    try:
        return getattr(request, "body", None)
    except BaseException:
        return None


# -------------------------------------------------------------- response side

def _note_response(capture: _capture.Capture, record: Any, response: Any) -> None:
    status = getattr(response, "status_code", None)
    if not isinstance(status, int):
        status = None
    pairs = _response_headers(response)
    final_url = _response_url(response)

    # Content metadata drives the body decision and must come from the raw
    # headers, before any redaction.
    content_type = _header_value(pairs, "content-type")
    content_encoding = _header_value(pairs, "content-encoding")
    with record.lock:
        record.response_type = content_type
        record.response_encoding = content_encoding

    capture.note_response(
        record, status, pairs, final_url, to_headers_seconds=_headers_seconds(response)
    )


def _response_headers(response: Any) -> List[Tuple[str, str]]:
    pairs: List[Tuple[str, str]] = []
    try:
        headers = getattr(response, "headers", None)
        if headers is not None:
            for name, value in headers.items():
                pairs.append((name, value))
    except BaseException:
        pass
    return pairs


def _header_value(pairs: List[Tuple[str, str]], name: str) -> str:
    wanted = name.lower()
    for header, value in pairs:
        if header.lower() == wanted:
            return value or ""
    return ""


def _response_url(response: Any) -> str:
    try:
        return str(getattr(response, "url", "") or "")
    except BaseException:
        return ""


def _headers_seconds(response: Any) -> Optional[float]:
    """requests records the time to headers on the response.

    It is measured before ``Session.send`` consumes the body, so using it keeps
    the header/body split honest even though capture sees the response after the
    body has already been read.
    """
    try:
        elapsed = getattr(response, "elapsed", None)
        if elapsed is None:
            return None
        seconds = elapsed.total_seconds()
    except BaseException:
        return None
    if seconds < 0:
        return None
    return seconds


def _observe_body(capture: _capture.Capture, record: Any, response: Any) -> None:
    # A non-streamed request has already been read by requests, so the body is
    # in memory and can be described without another network call.
    content = getattr(response, "_content", False)
    consumed = bool(getattr(response, "_content_consumed", False))
    if consumed and isinstance(content, (bytes, bytearray)):
        _capture._observe(capture, record, bytes(content), True)
        return
    if consumed:
        # Consumed but no inspectable value (for example a status with no raw
        # response): there is nothing to show, and the exchange is complete.
        capture.finish(record)
        return
    _wrap_streaming(capture, record, response)


def _wrap_streaming(capture: _capture.Capture, record: Any, response: Any) -> None:
    """Observe a streamed response as the application reads it.

    Only ``iter_content`` is wrapped: ``Response.content``, ``text``, ``json``,
    ``iter_lines`` and ``read``-style helpers all go through it, so one wrapper
    covers the documented read paths without pre-reading anything.
    """
    original_iter = getattr(response, "iter_content", None)
    if callable(original_iter):
        def iter_content(chunk_size: Any = 1, decode_unicode: bool = False,
                         _original: Any = original_iter) -> Any:
            for chunk in _original(chunk_size, decode_unicode):
                _capture._observe(capture, record, _as_bytes(chunk), False)
                yield chunk
            _capture._observe(capture, record, b"", True)
        try:
            response.iter_content = iter_content  # type: ignore[method-assign]
        except BaseException:
            pass

    original_close = getattr(response, "close", None)
    if callable(original_close):
        def close(_original: Any = original_close) -> Any:
            try:
                return _original()
            finally:
                capture.finish(record)
        try:
            response.close = close  # type: ignore[method-assign]
        except BaseException:
            pass


def _as_bytes(chunk: Any) -> bytes:
    if isinstance(chunk, bytes):
        return chunk
    if isinstance(chunk, (bytearray, memoryview)):
        return bytes(chunk)
    if isinstance(chunk, str):
        return chunk.encode("utf-8")
    return b""
