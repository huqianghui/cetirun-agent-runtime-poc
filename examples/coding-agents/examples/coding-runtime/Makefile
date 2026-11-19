# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

PYTHON ?= python3

.PHONY: test check demo
test:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m unittest discover -s tests -v

check:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -c "import ast, pathlib; [ast.parse(p.read_text(), filename=str(p)) for p in pathlib.Path('.').rglob('*.py')]"

demo:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) runtime.py serve --runtime copilot
