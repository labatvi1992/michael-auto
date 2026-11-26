#!/usr/bin/env python3
"""Entrypoint for Michael Auto CUA JSON-RPC stdio sidecar."""

from __future__ import annotations

import os
import sys

# Ensure parent 'sidecar/' directory is importable without shadowing site-packages 'cua_driver'.
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_SIDECAR_ROOT = os.path.dirname(_THIS_DIR)
if _SIDECAR_ROOT not in sys.path:
    sys.path.insert(0, _SIDECAR_ROOT)
# Remove current package dir from sys.path[0] when invoked directly as script.
if sys.path and os.path.abspath(sys.path[0]) == _THIS_DIR:
    sys.path.pop(0)

if "--jsonrpc-stdio" not in sys.argv and len(sys.argv) > 1:
    print("Usage: python main.py --jsonrpc-stdio", file=sys.stderr)
    sys.exit(1)

from michael_cua_sidecar.server import main  # noqa: E402

if __name__ == "__main__":
    main()
