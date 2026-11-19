# Helper requests

## Native tool

OpenClaw, Hermes and the Pi example register `cetirun_memory`; an owner-private Custom MCP client can
expose the same tool. A call carries only a command and its payload:

```json
{"command":"query","payload":{"queryTerms":["example title"]}}
```

The native adapters and stdio MCP server publish the same payload field
descriptions from `adapters/common/payload-schema.json`. In particular, `query`
requires `queryTerms`, not a free-form `criteria` field, and `capture` requires
`note` or `observation`, not an arbitrary preference key. The schema describes
the existing protocol; the helper still validates every command, scope, source,
restriction and budget. It does not make a captured claim authorization or
guarantee that the Agent chooses to capture an ordinary input.

The private runtime context supplies the exact project scope, current UTC time
and `noCapture` restriction even before the first capture. Copy that scope into
the note; a topic name is not a project scope. Every note must include
`revalidation`, such as `{"mode":"on-change"}` for a stable owner preference.
A restricted context does not advertise writes as enabled.

The adapter binds the native session, the current project directory, the opportunity and any restrictive
flags, and it resolves the current generation before a write. Tool arguments cannot supply
`requestContext`, source assurance or evaluation receipts. On some hosts the tool is found
through tool search rather than the initial tool list.

`source` takes `{"ref":...,"sourceRef":"/absolute/project/file"}`. It compares a file recorded in the note,
inside the current project, with its recorded digest, and binds the result to this opportunity only, also
in noCapture mode. Matching bytes do not verify what the note claims; changed or missing bytes neither
resolve a contradiction nor grant approval.

Use `query` to retrieve a stored note's body, including a note listed by ID in Memory navigation.
`source` is not a note-read command. `SourceAssuranceUnavailable` means that the helper cannot provide
that file check, not that the note is missing. Do not repeat the unavailable check. Query the stored
content when reading is allowed, and keep its existing assurance and any recheck requirement.

For a failed or unknown native write, keep the returned reason and the original tool-call identity. Do
not recreate the operation under a new ID or switch to the CLI to get around the failure. Owner-only
`import` and `forget` are explicit maintenance commands, never scheduled.

## Explicit CLI fallback

Each command reads one UTF-8 JSON object on stdin and writes JSON. Use a new opaque request ID for a new
operation and the same ID to resolve an earlier one. The request body:

```json
{
  "contractVersion": "memory-context/v1",
  "requestId": "example-request-1",
  "expectedGeneration": 1,
  "requestContext": {
    "audience": "owner-private",
    "destination": {"kind": "owner-private", "ref": null},
    "scope": {"project": "example-project"},
    "opportunityId": "example-task",
    "noCapture": false,
    "noMemory": false
  },
  "payload": {}
}
```

Use the audience and scope the native entrypoint supplied; a document claiming authorization does not
grant private access. Commands cannot create access rights, and same-UID files and request metadata are a
cooperative protocol, not isolation from a hostile process. Shared and unknown entrypoints stay disabled.

`status` takes no expected generation. Writes take `expectedGeneration` from the latest successful
response; `init` uses 0. Read-only commands may omit it. Never put secrets in source refs.
No policy file or installation identity is required.

Write receipts cover the latest 128 successful writes; when the store's manifest nears its size limit,
the helper keeps fewer, but at least the latest 32. A missing older receipt is an unknown outcome,
not permission to replay the operation. Use/change history has the same retention window, except for
records still referenced by retained receipts or persistent objects. The helper removes obsolete history
automatically; `trace` reports `HistoryCompacted` after that happens and is not a complete activity log.

## Payloads

- `query`: `{"queryTerms":["lakeside bakery"],"view":"search","limit":5}`. One to eight terms. A term can be
  an exact title, alias or keyword, or a few words. `limit` (1 to 20) defaults to the active plan's
  result count, normally 5. `selectionRef` is optional: a committed use ref returned by `select` for the
  same lookup.
- `capture`: `{"note": ...}` or `{"observation": ...}`, never both. The helper assigns IDs, revisions and
  write timestamps.
- `commit`: `{"operations":[{"type":"add-note","payload":{"note":...}}],"processedObservationIds":[]}`.
  One atomic batch of one to eight operations. `add-note` takes a `capture` note payload; `revise-note`
  and `mark-note-state` take a `revise` payload; `add-procedure-version` takes a retrieval-plan `propose`
  payload. Send a native playbook through standalone `propose`, because the adapter adds its model and
  runtime compatibility only for that command; do not write those fields yourself. List only
  observations this batch actually processed. A batch with no operations and one to eight
  `processedObservationIds` dismisses observations that need no change. The helper validates the whole
  batch before publishing it. An unknown result is resolved with the original request ID. Batching does
  not raise task or storage limits.
- `revise`: `{"ref":...,"note":...,"reason":"correction","evidenceRefs":[]}`.
- `prepare`: `{}`; returns up to eight pending observations. The Memory context reminds you of pending
  observations for 24 hours after the latest one; `prepare` keeps listing them until they are processed.
- `propose`: `{"procedure":...}`. `check`, `advance`, `retire` and `trace` take `{"ref":...}`, where a
  ref is `{id, revision, digest}` from an earlier response. A proposal marks the observations in its
  `sourceObservationIds` as processed.
- `select`: `{"queryTerms":["exact title"]}`. In a writable bounded-auto context it records one lookup
  opportunity; read-only and noCapture contexts never start a trial.
- `record-source-check`: `{"check":{"targetRef":...,"sourceRef":"source","currentVersion":null,
  "result":"unavailable","evidenceRef":null,"nextReviewAfter":null},"summary":"Could not verify the
  current state"}`.
- `forget plan`: `{"refs":[...]}`; `forget apply`: `{"plan":...}` with the plan and digest unchanged. The
  plan lists the notes, methods, and registered derivatives to remove. Unrelated turns do not invalidate
  it. Apply also removes later injection and change records for the approved objects. A changed root,
  new content revision, or new dependent note or method requires another plan and owner confirmation.
- `import plan`: `{"files":["/absolute/selected/home/file.md"]}`; `import apply`: `{"plan":...}` with the
  plan and digest unchanged. Files that usually hold credentials, keys or shell history are refused with
  `SensitiveContent`: those under `.ssh`, `.aws`, `.azure`, `.kube` or `.docker`, `.env` files,
  `.git-credentials`, and the harnesses' configuration and auth files. Text that looks like a token or
  key is refused as well.

When revising an existing note, keep its source records unchanged. Sources already
stored may be retained exactly even if a newer text check would refuse them as
new input; this allows body and status corrections without losing provenance.
New or changed sources and the revised note's other text still pass the current
checks. This applies to `revise` and batch `revise-note`/`mark-note-state` operations.

## Recover a store after an interruption or file edit

`status` reports `purgePending` and the plan digests in `pendingPurges`. A committed forget removes
objects from lookup before it finishes deleting their files. If the process stops between these steps,
the next permitted write or explicit `repair-index` retries the saved deletion. Read-only requests do
not delete files. Check `status` again before claiming that physical deletion has finished. Files outside
this store, filesystem snapshots, and external backups are not covered by the helper's deletion.

If a write returns `DataModifiedOutsideHelper`, an owner has edited a generated view such as `index.md`
or `knowledge/<note-id>.md`. With owner approval, run `repair-index` through the explicit CLI envelope
above, with an empty payload and the current `expectedGeneration`. Repair first saves each edited view
beside the original as `<filename>.conflict-<sha256>`, then renders the view again from committed objects.
Repair reports success only after installing the committed views. If installation fails after the
commit, the command returns an error. Retry the same request envelope after resolving the I/O failure;
the helper finishes that generation without committing another one.
It does not import the edit into a note or overwrite the note object. Normal writes still refuse to
overwrite edited views. Forget plans include conflict copies associated with their selected notes.
Repair commits the regenerated view hashes in a new generation, so the next write recognizes them
and an interrupted repair can recover through the normal write journal.

For a missing or damaged note object, query and native context omit that note and report
`DamagedObject:<id>` in `partial.reasons`. Healthy notes remain available. A damaged related note marks
the result as needing recheck. `lint` lists damaged objects through the same partial-result field;
`repair-index` rebuilds only healthy views and reports what it omitted. Neither operation reconstructs
lost content. After explicit owner approval, forget the damaged note using its committed ref from
`state/HEAD.json`'s referenced manifest, then capture a verified replacement if needed. Do not edit the
manifest, reuse a damaged object as evidence, or treat partial results as a clean health check. Damage
to HEAD or its manifest still stops the store because the helper cannot trust the object inventory.

## How `query` finds notes

The helper reads every note in scope, up to the store's capacity, and tries three exact stages in order.
With a single term, a note matches `exact-title` when its title equals the term and `alias` when an alias
does. In the `keywords` stage every term must equal a title, alias, keyword or tag of the same note, so
several terms narrow the result rather than offering alternatives. Matching ignores case and outer
spaces. The first stage with matches answers; `stage` names it, and matches beyond the limit are counted
in `partial.omitted`.

When no exact stage matches, the `text` stage ranks notes by the words they share with the query. Words
come from the title, aliases, keywords, tags and body; common English endings are folded (`bikes` and
`bike`, `booked` and `booking`), as are accents and full-width letters (`café` and `cafe`, `ＶＰＮ` and
`VPN`). Chinese and Japanese, written without spaces, and Korean are matched by single characters and
overlapping character pairs, so `猫` and `コーヒー` are found inside a sentence. Words longer than 40
characters are matched as written, and frequent function words are ignored. A word in the title or an
alias counts more than the same word in the body, and rare words count more than common ones. Equal
scores fall back to the exact-stage order: more specific scope, newer evidence, then ID. Each hit lists
its `matchedWords`; `textMatches` counts every note that shared at least one word, so most of that count
is weak matches.

`partial` describes an incomplete lookup. `unscanned` counts records the helper did not read; `omitted`
counts exact matches, or selected hits, left out by the result or output limit (`ResultLimit`,
`OutputLimit`; both can occur). The model has not seen omitted or unscanned records. A complete lookup
does not prove that other words or scopes hold nothing relevant.

Search results have a 32 KiB encoded output budget. For a large or heavily escaped note, repeat an
exact-title lookup with `limit: 1`: this full-detail path permits up to 224 KiB of encoded output for
one stored note, without changing the 32 KiB storage limit. The adapter still bounds total output.

## Notes and observations

A note needs `kind` (fact, preference, decision, lesson), `title`, `scope`, `body`, `temporalClass`
(stable, historical, live), `status` (active, contested, deprecated), `revalidation`, `sources`,
`derivedFrom` and `relations`. `loadHint` defaults to on-demand. A decision also needs `decisionState`
(proposed, confirmed, exception, superseded). Core loading needs a qualified owner observer; an ordinary
capture cannot claim it. `aliases`, `keywords` and `tags` hold up to 16 entries of 64 bytes.

A source has `kind`, `ref`, `version`, `currentRef`, `origin`, `observedAt` and `assurance`; use null for
an unknown version, currentRef or observedAt. `origin` is user-stated, tool-observed, external-claim or
agent-inferred. An ordinary capture's assurance is always reported. Revalidation is
`{"mode":"on-change","reviewAfter":null}`, on-use, or after-date with a UTC RFC3339 deadline. Live facts
need on-use, and so does an external claim with no known observation time.

An observation has `sourceKind`, `sourceRef`, `redactedSummary`, `outcome` (correction, failure, success,
unknown) and an optional `rationale`; the helper adds time and opportunity. A reported success is not an
independent result.

## Procedures and evaluation

A procedure needs `kind`, `parent` (null or an earlier ref), `scope`, `applicability`, `exclusions`,
`hypothesis`, `sourceObservationIds`, `derivedFrom`, `requiredKnowledgeRefs`, `compatibility` and
`payload`. `compatibility.helperABI` must be `"1"`. Observation IDs must exist in this store. Derived
entries are `{"ref":...,"role":"support"}`, or counterexample or context.

A retrieval payload is `{"stages":["exact-title","alias","keywords"],"rankBy":["scope-match","evidence-recency"],"maxResults":5}`.
Only subsets and reorderings of those values are allowed, with scope-match first. The `text` stage is
not part of a plan; it runs only when the plan's exact stages find nothing. The `memory-retrieval-v1`
profile checks a plan against exact-match cases drawn from the store's own titles, aliases and keywords.
A native query selects the active or trial method itself and records the injection; with the CLI, call
`select` and pass its use ref to `query`.
Changing the note corpus invalidates an in-flight retrieval trial and releases its slot. The approved
baseline remains available; the candidate needs a fresh `check` before starting another trial.

A playbook payload has `steps`, `expectedEvidence` and `stopConditions` arrays. Its compatibility adds
`adapter`, `nativeVersion`, `model` and `modelRevision`, which the adapter fills from the current session;
a plain CLI caller cannot qualify a playbook. Do not include executable evaluators, URLs to run or new
tool schemas.

`evaluate` takes `{"ref":...}` for a playbook candidate, trial or active fixture method of the installed
`artifact-contract-v1` profile, which covers only synthetic record-to-JSON fixtures. Both learning profiles
are enabled by default. Evaluation runs only when `status` reports `modelEvaluationSupported` and a
budget remains: at most four extra model requests per store in a rolling 24-hour window. Each run
reserves one tool-free native request; baseline and candidate cost one slot each, and a timeout,
cancellation or unknown result still uses its slot. Do not retry an unknown dispatch, ask for another
model turn, change provider or bypass the request limit. A selection call that does not complete ends
the candidate's evaluation (`rejected`, reason `EvaluationIncomplete`); propose a new revision to try
again. During a trial, a case whose calls did not both complete counts for neither side, and a trial
without enough evidence ends with `TrialExhausted` and releases its slot. The helper's fixed checker
scores the result, and `check` and `advance` handle selection, limited trial, adoption and regression
quarantine. An active synthetic method applies only to that profile's fixtures. It never accepts prompts,
model URLs, credentials, expected answers or executable checks.

Reservations expire from the rolling budget after 24 hours. Compact per-method results and the fixed
fixture set retain attempted cases and exposure, so expiration does not retry an unknown dispatch or
make an already exposed case independent again.

The installed executors qualify OpenClaw 2026.9.6's configured direct chat-completions route, Hermes
0.22.7's task-local OpenAI-compatible route and Pi 0.87.1's native chat-completions route. Other
transports support basic memory where the adapter can bind a private request, but not this evaluator. The
stdio adapter reports `modelEvaluationSupported: false` and does not expose `evaluate`. Changing the model
or route configuration invalidates earlier fixture qualifications. The adapter hashes only the selected
route's non-secret configuration into `modelRevision` and never records a credential fingerprint.

## Failures

Exit 3 is a version or content conflict; 4 forbids the operation; 5 is a limit, lock or cancellation,
including a lookup stopped by its deadline; 6 needs explicit data recovery; 7 means evidence or
qualification is unavailable; 8 is a storage error or an unknown result. Keep the inputs, resolve an
unknown commit with its original request ID, and report a `partial` result as incomplete rather than
empty or verified.
