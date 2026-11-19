// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
)

func TestCustomImageUserApprovalValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		user     CustomImageUser
		foreign  bool
		accepted bool
	}{
		{name: "approved", user: CustomImageUser{UID: 2000, GID: 2000}, accepted: true},
		{name: "root-user", user: CustomImageUser{GID: 2000}},
		{name: "root-group", user: CustomImageUser{UID: 2000}},
		{name: "negative-user", user: CustomImageUser{UID: -1, GID: 2000}},
		{name: "negative-group", user: CustomImageUser{UID: 2000, GID: -1}},
		{name: "proxy-user", user: CustomImageUser{UID: egresscapture.ProxyUID, GID: 2000}},
		{name: "large-user", user: CustomImageUser{UID: 2147483648, GID: 2000}},
		{name: "large-group", user: CustomImageUser{UID: 2000, GID: 2147483648}},
		{name: "unapproved-image", user: CustomImageUser{UID: 2000, GID: 2000}, foreign: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, agent := providerFixture(t)
			config := provider.config
			image := *agent.Spec.Agent.Image
			if test.foreign {
				image = "registry.example.com/foreign@sha256:" + strings.Repeat("b", 64)
			}
			config.ImageUsers = map[string]CustomImageUser{image: test.user}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseCustomRuntimeConfig(string(raw))
			if (err == nil) != test.accepted {
				t.Fatalf("image-user approval accepted=%t: %v", test.accepted, err)
			}
		})
	}
}

func TestCustomImageUserRequiresAnExplicitApprovedImage(t *testing.T) {
	provider, agent := providerFixture(t)
	if _, err := provider.ImageUser(agent); err == nil {
		t.Fatal("missing image-user approval supplied a default UID")
	}
	user := CustomImageUser{UID: 2000, GID: 2000}
	provider.config.ImageUsers = map[string]CustomImageUser{*agent.Spec.Agent.Image: user}
	if actual, err := provider.ImageUser(agent); err != nil || actual != user {
		t.Fatal("approved image user was not resolved:", err)
	}
	provider.config.ApprovedImages = nil
	if _, err := provider.ImageUser(agent); err == nil {
		t.Fatal("removed image approval still authorized its numeric user")
	}
}
