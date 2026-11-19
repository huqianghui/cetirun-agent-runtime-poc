// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeContextDoesNotLoadUnknownAudienceOrInheritedTasks(t *testing.T) {
	f := newFixture(t)
	n := testNote("private-title")
	n.Scope = Scope{"project": "/fixture/project"}
	request := f.request(t, captureInput{Note: &n})
	request.RequestContext.Scope = n.Scope
	if response, code := f.store.Execute(context.Background(), "capture", request); code != 0 {
		t.Fatal(response)
	}
	for _, input := range []NativeInput{{Platform: "slack", CWD: "/fixture/project"}, {Platform: "cli", CWD: "/fixture/project", ParentSessionID: "parent"}, {Platform: "cli", CWD: "/fixture/project", NoMemory: true}} {
		raw, _ := json.Marshal(input)
		text := f.store.NativeContext(context.Background(), "hermes", raw, "canonical")
		if strings.Contains(text, "private-title") {
			t.Fatal("private title leaked", text)
		}
	}
	raw := []byte(`{"cwd":"/fixture/project","extra":{"platform":"cli"}}`)
	text := f.store.NativeContext(context.Background(), "hermes", raw, "canonical")
	if !strings.Contains(text, "private-title") || !strings.Contains(text, "never authorization") {
		t.Fatal(text)
	}
}
