# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import os
from pathlib import Path


def register(ctx):
    selected = os.environ.get("CETIRUN_MEMORY_ASSETS")
    if not selected:
        return
    root = Path(selected)
    if not root.is_absolute():
        raise ValueError("MemoryBindingInvalid")
    spec = importlib.util.spec_from_file_location("cetirun_memory_adapter", root / "adapters/hermes/__init__.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.register(ctx)
