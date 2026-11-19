// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package assets

import "embed"

//go:embed AGENTS.md skills/cetirun-memory/SKILL.md skills/cetirun-memory/references/requests.md adapters/common/*.mjs adapters/common/*.json adapters/openclaw/* adapters/hermes/*.py
var Files embed.FS
