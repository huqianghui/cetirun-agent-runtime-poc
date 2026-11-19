// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import "github.com/Azure/CetiRun/app/internal/chataccess"

func (p *PreparedRelease) SupportsPortalChat() bool {
	return p.chart != nil && p.chart.Metadata != nil && p.chart.Metadata.Annotations[chataccess.ContractAnnotation] == "v1"
}
