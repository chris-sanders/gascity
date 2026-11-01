"""Research-only memoization wrapper around GitPython's official path validator."""
from atexit import register
from functools import lru_cache
import importlib
import os
import sys

_git_util = importlib.import_module("git.util")
_ORIGINAL = _git_util._validate_repo_path


@lru_cache(maxsize=8192)
def _validate_cached(path, mode, platform):
    return _ORIGINAL(path, mode)


def validate_repo_path(path, mode=None):
    if type(path) is str and len(path) <= 256 and (mode is None or type(mode) is int):
        return _validate_cached(path, mode, os.name)
    return _ORIGINAL(path, mode)


def _patch_loaded_modules():
    patched = []
    for name, module in tuple(sys.modules.items()):
        if name.startswith("git") and getattr(module, "_validate_repo_path", None) is _ORIGINAL:
            module._validate_repo_path = validate_repo_path
            patched.append(name)
    required = {
        "git.util",
        "git.index.base",
        "git.index.fun",
        "git.objects.fun",
        "git.objects.submodule.base",
    }
    missing = sorted(required - set(patched))
    if missing:
        raise RuntimeError(f"validator references were not patched: {missing}")
    return patched


_PATCHED_MODULES = _patch_loaded_modules()


def install_validator(function):
    for name, module in tuple(sys.modules.items()):
        if name.startswith("git") and getattr(module, "_validate_repo_path", None) in (
            _ORIGINAL,
            validate_repo_path,
        ):
            module._validate_repo_path = function


def cache_info():
    return _validate_cached.cache_info()


def _report_cache_info():
    info = cache_info()
    print(
        "CACHE_INFO "
        + str(
            {
                "maxsize": info.maxsize,
                "currsize": info.currsize,
                "hits": info.hits,
                "misses": info.misses,
                "patched_modules": sorted(_PATCHED_MODULES),
            }
        ),
        flush=True,
    )


register(_report_cache_info)
