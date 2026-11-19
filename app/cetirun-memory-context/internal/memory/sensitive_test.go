// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReviseRetainsLegacySourceContent(t *testing.T) {
	for _, field := range []string{"ref", "currentRef", "version"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			note := testNote("legacy provenance")
			if err := note.validate(); err != nil {
				t.Fatal(err)
			}
			signed := "https://storage.example.com/note?sig=synthetic"
			switch field {
			case "ref":
				note.Sources[0].Ref = signed
			case "currentRef":
				note.Sources[0].CurrentRef = &signed
			case "version":
				version := syntheticToken("gh"+"p_", 36)
				note.Sources[0].Version = &version
			}
			// Seed the synthetic pre-upgrade record through the store writer:
			// current capture must refuse it, but its digest and lineage are valid.
			note.ID, note.Revision = newID("note"), 1
			note.UpdatedAt = timestamp(f.store.now())
			result, _, err := f.store.write(context.Background(), f.request(t, struct{}{}), "fixture-legacy", false, func(v *snapshot) (writeResult, error) {
				ref, err := v.save(note.ID, note.Revision, note)
				return writeResult{Refs: []Ref{ref}}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			ref := result.Refs[0]
			child := testNote("derived note")
			child.DerivedFrom = []Derived{{Ref: ref, Role: "support"}}
			childRef := f.note(t, child)
			originalSources := append([]Source(nil), note.Sources...)
			note.Body = "Corrected harmless body."
			ref = f.call(t, "revise", reviseInput{Ref: ref, Note: note, Reason: "Correct the body"}).Data.(writeResult).Refs[0]
			note.Revision = ref.Revision
			note.Status = "deprecated"
			payload, _ := encode(reviseInput{Ref: ref, Note: note, Reason: "Retire the statement"})
			op, _ := encode(operation{Type: "mark-note-state", Payload: payload})
			ref = f.call(t, "commit", batchInput{Operations: []json.RawMessage{op}}).Data.(writeResult).Refs[0]
			note.Revision = ref.Revision
			v, err := f.store.open()
			if err != nil {
				t.Fatal(err)
			}
			var stored Note
			err = v.read(ref, &stored)
			childPresent := v.m.Objects[childRef.ID] == childRef && !v.m.Tombstones[childRef.ID]
			v.fs.close()
			if err != nil || !reflect.DeepEqual(stored.Sources, originalSources) || stored.Body != note.Body || stored.Status != "deprecated" || !childPresent {
				t.Fatalf("revision lost provenance, changes or derivative: %+v, %v", stored, err)
			}

			for _, change := range []string{"body", "new source", "changed source", "assurance", "drop source"} {
				t.Run(change, func(t *testing.T) {
					changed := note
					changed.Sources = append([]Source(nil), note.Sources...)
					want := "InvalidInput"
					switch change {
					case "body":
						changed.Body = "API_KEY=" + syntheticToken("", 32)
					case "new source":
						extra := testNote("extra").Sources[0]
						extra.Kind = "other-fixture"
						extra.Ref = signed
						changed.Sources = append(changed.Sources, extra)
					case "changed source":
						changed.Sources[0].Ref = signed + "-changed"
					case "assurance":
						changed.Sources[0].Assurance = "adapter-observed"
					case "drop source":
						changed.Sources = testNote("replacement").Sources
						want = "SourceLineageRequired"
					}
					response, code := f.store.Execute(context.Background(), "revise", f.request(t, reviseInput{Ref: ref, Note: changed, Reason: "Synthetic revision"}))
					if code != 2 || response.Reason != want {
						t.Fatalf("%s: code %d, %+v", change, code, response)
					}
				})
			}
			copy := note
			copy.ID, copy.Revision, copy.UpdatedAt = "", 0, ""
			if response, code := f.store.Execute(context.Background(), "capture", f.request(t, captureInput{Note: &copy})); code != 2 || response.Reason != "InvalidInput" {
				t.Fatalf("new capture accepted legacy credential provenance: %+v", response)
			}
		})
	}
}

// syntheticToken builds a token-shaped value at run time, so that this source
// holds no string in a credential format.
func syntheticToken(prefix string, length int) string {
	return prefix + strings.Repeat("Synthetic0", length/10+1)[:length]
}

func TestSecretDetection(t *testing.T) {
	segment := func(part string) string { return base64.RawURLEncoding.EncodeToString([]byte(part)) }
	refused := map[string]string{
		"private key":         "-----BEGIN " + "PRIVATE KEY-----",
		"bearer token":        "Authorization: Bearer " + syntheticToken("", 24),
		"OpenAI key":          syntheticToken("sk-"+"proj-", 32),
		"GitHub token":        syntheticToken("gh"+"p_", 36),
		"GitHub OAuth token":  syntheticToken("gh"+"o_", 36),
		"GitHub fine-grained": syntheticToken("github"+"_pat_", 40),
		"GitLab token":        syntheticToken("gl"+"pat-", 20),
		"Slack token":         "xo" + "xb-1234567890-" + syntheticToken("", 24),
		"AWS key ID":          "AK" + "IA" + "SYNTHETIC0123456",
		"Google API key":      syntheticToken("AI"+"za", 35),
		"npm token":           syntheticToken("np"+"m_", 36),
		"JSON Web Token":      segment(`{"alg":"none"}`) + "." + segment(`{"sub":"synthetic"}`) + "." + segment("synthetic signature"),
		"URL credentials":     "https://x-access-token:" + syntheticToken("", 20) + "@git.example.com/repo.git",
		"signed URL":          "https://storage.example.com/container/blob?sv=2025-01-01&s" + "ig=" + syntheticToken("", 20),
		"variable":            "OPENAI_API" + "_KEY=" + syntheticToken("", 32),
		"exported token":      "export GITHUB" + "_TOKEN=" + syntheticToken("", 32),
		"JSON token":          `{"gateway":{"auth":{"token":"` + syntheticToken("", 32) + `"}}}`,
		"YAML token":          "github.com:\n    oauth_token: " + syntheticToken("", 32),
		"header":              "curl -H 'api-key: " + strings.Repeat("0123456789abcdef", 2) + "' https://api.example.com",
		"connection string":   "DefaultEndpointsProtocol=https;AccountName=example;Account" + "Key=" + syntheticToken("", 40) + "==",
		"password":            "password = " + syntheticToken("", 20),
	}
	for name, text := range refused {
		if validText(text, 32<<10) {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
	for _, text := range []string{
		"The owner prefers green tea in the morning.",
		"max_tokens: 4096; token budget: 128000 tokens per day",
		"tokenizer: o200k_harmony_extended",
		"password: at least 12 characters, rotated every 90 days",
		"OPENAI_API_KEY=${OPENAI_API_KEY}",
		"credentials_file: /home/vibe/.config/example/credentials.json",
		"token: https://login.example.com/oauth2/v2.0/token",
		"secret: 2026-10-13T09:20:57Z",
		"ssh://git@git.example.com:22/example/repo.git and http://localhost:8080/api",
		"commit 0123456789abcdef0123456789abcdef01234567, digest sha256:" + strings.Repeat("0a", 32),
		"The Wi-Fi password is on the fridge; the API key is in the team vault.",
		"会议定在 2026-10-20，地点在上海。",
	} {
		if !validText(text, 32<<10) {
			t.Errorf("refused ordinary text %q", text)
		}
	}
}

func TestImportRefusesCredentialFiles(t *testing.T) {
	f := newFixture(t)
	write := func(rel, content string) string {
		t.Helper()
		name := filepath.Join(f.home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	importPlan := func(name string) (Response, int) {
		return f.store.Execute(context.Background(), "import-plan", f.request(t, importInput{Files: []string{name}}))
	}
	// Plain content: these files are refused for where they are, not for what
	// they hold.
	for _, rel := range []string{
		".ssh/id_ed25519", ".ssh/config", ".gnupg/gpg.conf", ".aws/credentials", ".azure/msal_token_cache.json",
		".kube/config", ".docker/config.json", ".config/gh/hosts.yml", ".config/github-copilot/apps.json",
		".git-credentials", ".netrc", ".npmrc", ".env", ".hermes/.env", "project/.env.local", ".bash_history",
		".local/share/fish/fish_history", ".hermes/auth.json", ".hermes/config.yaml", ".openclaw/openclaw.json",
		".openclaw/openclaw.json.bak", ".openclaw/credentials/channel.json", ".openclaw/agents/main/agent/auth-profiles.json",
		".openclaw/agents/main/agent/models.json", "certificates/server.pem", "certificates/server.key",
		"Downloads/client_secret_example.json", "kubeconfig",
	} {
		if response, code := importPlan(write(rel, "Synthetic plain text.")); code != 2 || response.Reason != "SensitiveContent" {
			t.Errorf("%s: code %d, reason %s; want SensitiveContent", rel, code, response.Reason)
		}
	}
	// The harnesses' own memory files and other notes in HOME are imported.
	files := []string{}
	for _, rel := range []string{".openclaw/workspace/MEMORY.md", ".openclaw/workspace/memory/2026-10-01.md",
		".hermes/memories/MEMORY.md", "notes/environment.md", "notes/keyboard-shortcuts.md"} {
		files = append(files, write(rel, "Synthetic note: the owner prefers green tea."))
	}
	plan := f.call(t, "import-plan", importInput{Files: files}).Data.(ImportPlan)
	if refs := f.call(t, "import-apply", importApplyInput{Plan: plan}).Data.(writeResult).Refs; len(refs) != len(files) {
		t.Fatalf("imported %d of %d note files", len(refs), len(files))
	}
	// Elsewhere, text with a credential is refused.
	if response, code := importPlan(write("notes/setup.md", "export GITHUB"+"_TOKEN="+syntheticToken("", 32))); code != 2 || response.Reason != "SensitiveContent" {
		t.Fatalf("a note with a token: code %d, reason %s; want SensitiveContent", code, response.Reason)
	}
	// Apply rechecks both path and content even for an internally consistent
	// plan supplied by a caller. A later refused file must not commit earlier
	// files from the same batch.
	for _, rel := range []string{".env", "notes/assignment.md"} {
		body := "API_KEY=" + syntheticToken("", 32)
		if rel == ".env" {
			body = "Synthetic plain text."
		}
		name := write(rel, body)
		plan := f.call(t, "import-plan", importInput{Files: []string{files[0]}}).Data.(ImportPlan)
		plan.Files = append(plan.Files, ImportFile{Path: name, Digest: digest([]byte(body))})
		plan.Digest = plan.hash()
		request := f.request(t, importApplyInput{Plan: plan})
		before := *request.ExpectedGeneration
		response, code := f.store.Execute(context.Background(), "import-apply", request)
		if code != 2 || response.Reason != "SensitiveContent" {
			t.Fatalf("apply %s: code %d, reason %s", rel, code, response.Reason)
		}
		if after := *f.request(t, struct{}{}).ExpectedGeneration; after != before {
			t.Fatalf("refused batch changed generation: %d -> %d", before, after)
		}
	}
}
