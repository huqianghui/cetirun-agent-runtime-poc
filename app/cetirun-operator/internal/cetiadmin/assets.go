// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"embed"
	"io/fs"
)

const PackagedHelmPath = "/usr/local/share/cetirun/cetiadmin/bin/helm"

//go:embed assets/skills assets/cron assets/scripts
var assets embed.FS

func BundledAssets() (fs.FS, error) {
	return fs.Sub(assets, "assets")
}
