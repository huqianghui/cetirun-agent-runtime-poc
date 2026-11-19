#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eu
/usr/local/bin/hermes-python /etc/cetirun/hermes-runtime.py check
exec hermes gateway run --replace --accept-hooks --external-supervisor
