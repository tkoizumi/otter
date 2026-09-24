"""httpx instrumentation for Otter HTTP capture.

The seam is ``httpx.Client.send`` and ``httpx.AsyncClient.send``. The module-level
helpers, ``Client.request`` and ``Client.stream`` all funnel through ``send``, and
httpx follows redirects internally by calling ``_send_single_request`` rather than
``send``, so one wrapper per client records each user-visible exchange exactly
once.

Async support is not an afterthought: an integration that uses ``AsyncClient``
must be captured the same way. The response is observed through its async read
paths, and the buffered body of a non-streamed response is inspected without
touching the stream again.

Unlike requests, httpx does not expose a time-to-headers value: it reads a
non-streamed body before ``send`` returns. Capture therefore reports the moment
the call returned, which is exact for a streamed response and an over-estimate of
the header time for a buffered one. The status, headers and body are unaffected.
"""

import os
import threading
from typing import Any, List, Optional, Tuple

from . import _capture

__all__ = ["NAME", "install", "uninstall"]

NAME = _capture.ADAPTER_HTTPX

_MISSING = object()

_install_lock = threading.Lock()
_original_send: Any = None
_original_async_send: Any = None
#: The active capture, or None. Deliberately not named ``_capture``.
_active_capture: Optional[Any] = None
#: httpx's Request type, used to skip a caller's misuse rather than recording it
#: as a transport failure.
_request_type: Any = None


def install(capture: _capture.Capture) -> bool:
    """Start capturing httpx, if it is installed in this interpreter."""
    global _original_send, _original_async_send, _active_capture, _request_type
    try:
        import httpx
    except BaseException:
        return False

    with _install_lock:
        _active_capture = capture
        _request_type = httpx.Request
        _capture.register_internal_frames(_package_dir(httpx))
        if _original_send is None:
            _original_send = httpx.Client.send
            httpx.Client.send = _capturing_send
        if _original_async_send is None:
            _original_async_send = httpx.AsyncClient.send
            httpx.AsyncClient.send = _capturing_send_async
    return True


def uninstall() -> None:
    """Stop capturing and restore the original ``send`` methods."""
    global _original_send, _original_async_send, _active_capture
    try:
        import httpx
    except BaseException:
        return
    with _install_lock:
        if _original_send is not None:
            httpx.Client.send = _original_send
            _original_send = None
        if _original_async_send is not None:
            httpx.AsyncClient.send = _original_async_send
            _original_async_send = None
        _active_capture = None


def _package_dir(module: Any) -> str:
    path = getattr(module, "__file__", "") or ""
    return os.path.dirname(path) if path else ""


def _capturing_send(self: Any, request: Any, **kwargs: Any) -> Any:
    original = _original_send
    capture = _active_capture
    if original is None or capture is None or _capture.is_suppressed():
        return original(self, request, **kwargs)
    if _request_type is not None and not isinstance(request, _request_type):
        return original(self, request, **kwargs)

    record = _begin(capture, request)
    try:
        response = original(self, request, **kwargs)
    except BaseException as exc:
        capture.note_transport_error(record, exc)
        raise

    _note_response(capture, record, response)
    _observe_body(capture, record, response, is_async=False)
    return response


async def _capturing_send_async(self: Any, request: Any, **kwargs: Any) -> Any:
    original = _original_async_send
    capture = _active_capture
    if original is None or capture is None or _capture.is_suppressed():
        return await original(self, request, **kwargs)
    if _request_type is not None and not isinstance(request, _request_type):
        return await original(self, request, **kwargs)

    record = _begin(capture, request)
    try:
        response = await original(self, request, **kwargs)
    except BaseException as exc:
        capture.note_transport_error(record, exc)
        raise

    _note_response(capture, record, response)
    _observe_body(capture, record, response, is_async=True)
    return response


def _begin(capture: _capture.Capture, request: Any) -> Any:
    return capture.begin(
        _request_method(request),
        _request_url(request),
        _request_headers(request),
        _request_body(request),
    )


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
        if headers is None:
            return pairs
        multi = getattr(headers, "multi_items", None)
        if callable(multi):
            for name, value in multi():
                pairs.append((name, value))
            return pairs
        for name, value in headers.items():
            pairs.append((name, value))
    except BaseException:
        pass
    return pairs


def _request_body(request: Any) -> Any:
    """Return the request body without consuming a stream.

    httpx keeps a buffered body in ``_content`` and a streamed one in ``stream``.
    Returning the stream object is what makes the capture layer describe it as an
    unreadable stream rather than silently reading it.
    """
    if hasattr(request, "_content"):
        try:
            return request.content
        except BaseException:
            return None
    try:
        return getattr(request, "stream", None)
    except BaseException:
        return None


# -------------------------------------------------------------- response side

def _note_response(capture: _capture.Capture, record: Any, response: Any) -> None:
    status = getattr(response, "status_code", None)
    if not isinstance(status, int):
        status = None
    pairs = _response_headers(response)
    final_url = _response_url(response)

    content_type = _header_value(pairs, "content-type")
    content_encoding = _header_value(pairs, "content-encoding")
    with record.lock:
        record.response_type = content_type
        record.response_encoding = content_encoding

    capture.note_response(record, status, pairs, final_url)


def _response_headers(response: Any) -> List[Tuple[str, str]]:
    pairs: List[Tuple[str, str]] = []
    try:
        headers = getattr(response, "headers", None)
        if headers is None:
            return pairs
        multi = getattr(headers, "multi_items", None)
        if callable(multi):
            for name, value in multi():
                pairs.append((name, value))
            return pairs
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


def _observe_body(capture: _capture.Capture, record: Any, response: Any,
                  is_async: bool) -> None:
    content = getattr(response, "_content", _MISSING)
    if content is not _MISSING and isinstance(content, (bytes, bytearray)):
        _capture._observe(capture, record, bytes(content), True)
        return
    if is_async:
        _wrap_async_streaming(capture, record, response)
    else:
        _wrap_sync_streaming(capture, record, response)


def _wrap_sync_streaming(capture: _capture.Capture, record: Any, response: Any) -> None:
    """Observe a sync stream through ``iter_raw``, the lowest decoded layer.

    ``read``, ``iter_bytes`` and ``iter_text`` all consume ``iter_raw``, so
    wrapping it covers them without observing the same bytes twice.

    httpx's own ``iter_raw`` closes the response before its iterator finishes, so
    the completion observe has to happen after that internal close. The response
    is marked while a read is in flight (``_otter_reading``) so the close wrapper
    does not finalize the exchange as "never read" halfway through a read; the
    stream wrapper finalizes it instead, and the finally clause covers a read
    abandoned part-way.
    """
    original_iter = getattr(response, "iter_raw", None)
    if callable(original_iter):
        def iter_raw(chunk_size: Any = None, _original: Any = original_iter) -> Any:
            response._otter_reading = True
            try:
                for chunk in _original(chunk_size):
                    _capture._observe(capture, record, _as_bytes(chunk), False)
                    yield chunk
                _capture._observe(capture, record, b"", True)
            finally:
                response._otter_reading = False
                if not record.finished:
                    capture.finish(record)
        try:
            response.iter_raw = iter_raw  # type: ignore[method-assign]
        except BaseException:
            pass

    original_close = getattr(response, "close", None)
    if callable(original_close):
        def close(_original: Any = original_close) -> Any:
            try:
                return _original()
            finally:
                if not getattr(response, "_otter_reading", False):
                    capture.finish(record)
        try:
            response.close = close  # type: ignore[method-assign]
        except BaseException:
            pass


def _wrap_async_streaming(capture: _capture.Capture, record: Any, response: Any) -> None:
    original_iter = getattr(response, "aiter_raw", None)
    if callable(original_iter):
        async def aiter_raw(chunk_size: Any = None, _original: Any = original_iter) -> Any:
            response._otter_reading = True
            try:
                async for chunk in _original(chunk_size):
                    _capture._observe(capture, record, _as_bytes(chunk), False)
                    yield chunk
                _capture._observe(capture, record, b"", True)
            finally:
                response._otter_reading = False
                if not record.finished:
                    capture.finish(record)
        try:
            response.aiter_raw = aiter_raw  # type: ignore[method-assign]
        except BaseException:
            pass

    original_close = getattr(response, "aclose", None)
    if callable(original_close):
        async def aclose(_original: Any = original_close) -> Any:
            try:
                return await _original()
            finally:
                if not getattr(response, "_otter_reading", False):
                    capture.finish(record)
        try:
            response.aclose = aclose  # type: ignore[method-assign]
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
