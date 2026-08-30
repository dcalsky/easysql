"""Thin Python binding for the easysql native library."""

from __future__ import annotations

import ctypes
import json
import os
import platform
from pathlib import Path
from threading import Lock
from typing import Any, Mapping

ABI_VERSION = 1
_instance: "EasySQL | None" = None
_instance_lock = Lock()


def _library_name() -> str:
    system = platform.system()
    if system == "Darwin":
        return "libeasysql.dylib"
    if system == "Windows":
        return "easysql.dll"
    if system == "Linux":
        return "libeasysql.so"
    raise RuntimeError(f"unsupported operating system: {system}")


def _default_library_path() -> Path:
    override = os.environ.get("EASYSQL_LIBRARY_PATH")
    if override:
        return Path(override).expanduser().resolve()
    return Path(__file__).resolve().parents[3] / "lib" / _library_name()


class EasySQL:
    """Loaded easysql library. Instances are safe to share between threads."""

    def __init__(self, library_path: os.PathLike[str] | str | None = None) -> None:
        path = Path(library_path).expanduser().resolve() if library_path else _default_library_path()
        self._lib = ctypes.CDLL(str(path))
        self._lib.easysql_abi_version.argtypes = []
        self._lib.easysql_abi_version.restype = ctypes.c_uint32
        self._lib.easysql_version.argtypes = []
        self._lib.easysql_version.restype = ctypes.c_void_p
        self._lib.easysql_execute.argtypes = [ctypes.c_void_p, ctypes.c_size_t]
        self._lib.easysql_execute.restype = ctypes.c_void_p
        self._lib.easysql_free_string.argtypes = [ctypes.c_void_p]
        self._lib.easysql_free_string.restype = None

        actual = int(self._lib.easysql_abi_version())
        if actual != ABI_VERSION:
            raise RuntimeError(f"easysql ABI {actual} is incompatible with binding ABI {ABI_VERSION}")

    def _consume(self, pointer: int | None) -> str:
        if not pointer:
            raise RuntimeError("easysql returned a null response")
        try:
            return ctypes.string_at(pointer).decode("utf-8")
        finally:
            self._lib.easysql_free_string(pointer)

    @property
    def version(self) -> str:
        return self._consume(self._lib.easysql_version())

    def execute(self, request: Mapping[str, Any] | str | bytes) -> dict[str, Any]:
        if isinstance(request, Mapping):
            payload = json.dumps(request, separators=(",", ":")).encode("utf-8")
        elif isinstance(request, str):
            payload = request.encode("utf-8")
        else:
            payload = bytes(request)

        buffer = ctypes.create_string_buffer(payload)
        response = self._consume(self._lib.easysql_execute(buffer, len(payload)))
        return json.loads(response)


def client() -> EasySQL:
    global _instance
    if _instance is None:
        with _instance_lock:
            if _instance is None:
                _instance = EasySQL()
    return _instance


def version() -> str:
    return client().version


def execute(request: Mapping[str, Any] | str | bytes) -> dict[str, Any]:
    return client().execute(request)


__all__ = ["ABI_VERSION", "EasySQL", "client", "execute", "version"]
