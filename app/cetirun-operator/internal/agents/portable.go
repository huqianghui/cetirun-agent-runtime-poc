// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agents

import (
	"fmt"
	"net/url"
)

func ValidatePortableBindings(bindings Bindings) error {
	if bindings.ContextRevision == "" {
		return fmt.Errorf("ContextBindingPending: approved context is required")
	}
	base, err := url.Parse(bindings.Model.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("ModelBindingInvalid: API base must be an HTTP URL without credentials")
	}
	if bindings.Model.CredentialSlot != "model" || bindings.Model.Revision == "" {
		return fmt.Errorf("ModelBindingPending: instance model access is required")
	}
	return nil
}
