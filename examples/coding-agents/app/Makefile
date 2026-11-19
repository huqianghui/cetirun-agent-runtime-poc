# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

PYTHON ?= python3
.PHONY: test check build
test:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -m unittest discover -s tests -v
check:
	PYTHONDONTWRITEBYTECODE=1 $(PYTHON) -c "import ast,pathlib,tomllib,json; [ast.parse(p.read_text(), filename=str(p)) for p in pathlib.Path('.').rglob('*.py')]; [tomllib.loads(p.read_text()) for p in pathlib.Path('profiles').rglob('*.toml')]; json.loads(pathlib.Path('artifacts.json').read_text())"
build:
	docker buildx build --platform linux/amd64 --load --build-arg CODING_RUNTIME=copilot -t cetirun-copilot:local .
	docker buildx build --platform linux/amd64 --load --build-arg CODING_RUNTIME=claude -t cetirun-claude:local .
	docker buildx build --platform linux/amd64 --load --build-arg CODING_RUNTIME=codex -t cetirun-codex:local .
