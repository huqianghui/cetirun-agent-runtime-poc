// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCoreRequiresAnObservedOwnerCommand(t *testing.T) {
	f := newFixture(t)
	note := testNote("unattested")
	note.Kind = "preference"
	note.LoadHint = "core"
	note.Sources[0].Kind = "native-owner"
	note.Sources[0].Origin = "user-stated"
	f.note(t, note)
	if core := f.call(t, "query", Query{View: "core"}).Data.(QueryResult); len(core.Hits) != 0 {
		t.Fatal("ordinary capture forged owner observation")
	}
	raw, _ := json.Marshal(NativeInput{Platform: "cli", CWD: "fixture", SessionID: "owner-session", UserMessage: "Remember my preference: keep fixture reports short."})
	if result := f.store.NativeContext(context.Background(), "hermes", raw, "contract"); !strings.Contains(result, "keep fixture reports short.") {
		t.Fatal(result)
	}
	core := f.call(t, "query", Query{View: "core"}).Data.(QueryResult)
	if len(core.Hits) != 1 || core.Hits[0].Note.Sources[0].Assurance != "adapter-observed" {
		t.Fatal(core)
	}
	raw, _ = json.Marshal(NativeInput{Platform: "cli", CWD: "fixture", NoCapture: true, UserMessage: "Remember my preference: this should not be captured."})
	f.store.NativeContext(context.Background(), "hermes", raw, "contract")
	core = f.call(t, "query", Query{View: "core"}).Data.(QueryResult)
	if len(core.Hits) != 1 {
		t.Fatal("noCapture wrote a preference")
	}
	observed := core.Hits[0]
	corrected := observed.Note
	corrected.Body = "A proposed interpretation is not another observed owner statement."
	f.call(t, "revise", reviseInput{Ref: observed.Ref, Note: corrected, Reason: "correct the wording"})
	core = f.call(t, "query", Query{View: "core"}).Data.(QueryResult)
	if len(core.Hits) != 0 {
		t.Fatal("a changed statement inherited the original core eligibility")
	}
	q := f.call(t, "query", Query{QueryTerms: []string{corrected.Title}}).Data.(QueryResult)
	if len(q.Hits) != 1 || q.Hits[0].Note.Sources[0].Ref != observed.Note.Sources[0].Ref {
		t.Fatal("revision lost the original source lineage")
	}
}
