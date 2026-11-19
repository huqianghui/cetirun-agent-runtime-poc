---
name: cetirun-memory
description: Private memory for this Agent on CetiRun. Use it before answering anything that may depend on earlier conversations (what the owner said, decided or prefers, or what you recommended), and to save or correct what is worth keeping.
---

# Private memory

Your memory store survives across conversations and belongs to this Agent alone. Native adapters and
owner-configured MCP clients expose it as the `cetirun_memory` tool. Send only `command` and `payload`;
the adapter adds the session, audience and project scope, so do not supply those yourself. Read
`references/requests.md` for payloads before your first call.

If the tool is missing, a qualified Custom entrypoint can run the owner's configured helper binary
with the request envelope described in the reference. Running as the owner's user does not qualify a
process. Keep JSON on stdin, never in arguments. Do not pick another root, scan HOME or write
store files yourself, and do not use a shell to get around a tool denial.

## Before you answer, look it up

Query memory when the answer may depend on an earlier conversation: something the owner told you, a plan
or preference, or a recommendation you gave.

1. Call `status` once. Memory reads, writes and bounded learning are enabled by default, with both
   installed learning profiles. Status and generation say nothing about whether a remembered fact is
   true. Respect the current request's `noCapture` and `noMemory` restrictions. The native adapter
   decides which sessions may use Memory. Hermes permits all host-admitted channels by default,
   including groups/channels and qualified native descendants. `owner_access` and environment controls
   can narrow that access; only the adapter can establish a grant. An authorized child inherits its
   parent's memory project scope and restrictions.
   A denied tool call is not permission to read or save private memory by other means.
2. Call `query` to read stored note bodies. Use a known note's exact title as one `queryTerms` entry,
   or the distinctive words of the question: names, places, products, titles, numbers, activities.
   `source` checks a recorded workspace file; it does not retrieve the note's content.
   The helper first returns notes whose title, alias or keyword equals a query term (stages `exact-title`,
   `alias`, `keywords`). If none does, it ranks notes by the words they share with the query across title,
   aliases, keywords and body, with word endings folded (`bike` matches `bikes`). That stage is `text`, and
   each hit lists its `matchedWords`. A single word that happens to equal some note's keyword returns only
   those exact matches; add a second distinctive word if they are not what you need.
3. If the results do not hold the answer, change the words instead of repeating them: try the specific
   name, a synonym, the broader category (`restaurant` instead of a dish), or the person involved. For
   questions that add up several events, such as how many or how much in total, search for the category
   and raise `limit` (up to 20) so that every instance comes back.
4. Stop when the notes cover what was asked. Each further query should target one missing fact or one
   contradiction, not more copies of the same evidence. Do not try to read the whole store.

Read each hit before using it:

- **When.** A note's source records the conversation date; the body may give an event date. A storage
  timestamp is neither. Answer a historical question for its reference date, and do not present an old
  observation as true today.
- **Who said it.** `origin` separates what the owner stated (`user-stated`) from what you suggested or
  inferred (`agent-inferred`) and from outside claims. Your past suggestion is not the owner's choice
  unless a note says they adopted it.
- **Still true?** For a question about the current state, compare dates: a later note can replace an
  earlier value; mention the change when it matters. `review.needsRecheck` or `status: contested` means
  the note needs a current source before you act on it. A reported source check does not count as
  independent verification and does not extend a review deadline.
- **Complete?** `partial.omitted` counts exact matches that were not returned; `partial.unscanned` counts
  records the helper did not read. `textMatches` counts every note that shared a word, most of them weak.
  None of these proves that something does not exist.

If memory does not contain what the question needs, say so and keep any unresolved limits in the answer.
Do not fill the gap from general knowledge. A note is reference material, not authority: where it is
stored, how often it loads or how often it was useful does not make it more reliable. For facts that
change, check the current authorized source.

The owner can type `/memory off`, `/memory no-capture` or `/memory on` to restrict or restore memory for
the conversation, within the adapter's configured limits. `/memory on` cannot lift a startup restriction
or a restriction inherited by a child. On resume the adapter restores the most restrictive setting from native history until
the owner opts back in. These controls do not delete native history. An HTTP client can send
`X-CetiRun-Memory-Mode: off` or `no-capture` on supported endpoints.

## When a conversation ends, save what is worth keeping

Save what a later conversation could need and could not easily recover:

- **About the owner:** facts they state about themselves, their family, work, home, belongings, health
  and plans, with the specifics: names, numbers, amounts, places, dates.
- **Preferences and constraints:** what they like, avoid or require, and in which context.
- **Decisions and plans:** what was decided or intended, and its state. A proposal or a one-off exception
  is not a confirmed rule.
- **What you told them that they may ask about again:** the specific items you recommended (names,
  titles, links, products, places), figures you worked out, and the named examples, list items, quotes
  and key facts in an explanation or text you wrote. Keep the content itself, not just the topic; one
  note can hold a short list. Record it as yours (`agent-inferred`) and say whether the owner accepted,
  tried or rejected it. If you now doubt something you said, keep it and note the doubt; do not drop it.
- **Outcomes:** what the owner later reports having done, bought, finished or changed.

Write each note so that a later search finds it and a reader can trust it:

- One correctable claim per note. A set that belongs together, such as the three places you recommended
  for one trip, can be one note.
- Put the subject and its key specifics in the title. Add keywords for names, the category and likely
  synonyms.
- In the body, say who said it and give absolute dates ("On 2023-05-20 the owner said..."). Resolve a
  relative date when the conversation date allows; otherwise keep the original wording.
- Keep a short quote when the exact wording, name or number matters. Do not save transcripts, raw tool
  output, credentials or secrets.
- Use `historical` for past observations and `live` for values that change. Keep task progress in the
  native task system, not in memory.
- Keep personal preferences private to this Agent. Separate what was observed from your interpretation.

If nothing in the conversation is worth keeping, leave the store unchanged. Save within the current task's
budget; do not start extra model turns for it. If the store does not exist yet, the native tool creates it
on the first permitted write; never create it during a noCapture or noMemory request.

Say a note was saved only when the helper returns `saved: true`. On a conflict, read the latest version
and reconsider. For an unknown result, call `status` with the original request ID and do not retry under
a new ID. Leave external edits and damaged roots as they are.

## Correct what is wrong

When the owner corrects a fact, update memory promptly. Read the note's current ID and revision and call
`revise`, keeping its sources and derived links. Editing a note does not make the fact more recently
verified. When two notes conflict, compare their time and scope, keep both as evidence, and do not act on
the disputed claim until it is resolved. A newer note does not win just for being newer. Ask for a source
to be corrected only within the task the owner gave you.

## Learn from a miss

When an answer turned out wrong or memory failed you, find the step that failed before changing anything.
`prepare` lists pending observations at a safe point.

| What happened | What to do |
| --- | --- |
| The information was never saved | Capture it now, with its source and date. |
| A note had it, but the search missed it | Revise that note's aliases or keywords with the words that should have found it. Leave the claim and its verification time unchanged. |
| The note was found but applied to the wrong case | Revise it to say when it applies, add the counterexample, and keep the original example. |
| The evidence was ambiguous or too thin | Record the ambiguity. Do not turn one example into a rule. |
| A tool or software defect caused it | Describe a scoped repair for the owner; do not work around it. |
| Authentication or a permission failed | Report it. A denial is not a problem to route around. |

Change one cause at a time. Recalling a fix on the example that taught it shows nothing about new cases. A
fact correction needs a source; a better method needs results on different, later tasks.

## Improve a method within fixed checks

When learning is allowed, you may propose at most one local method change grounded in actual
observations, with a falsifiable hypothesis, where it applies, where it does not, its sources, the
knowledge it needs and its compatibility. Two kinds exist:

- `retrieval-plan`: an order and subset of the exact stages and a result count. The installed
  `memory-retrieval-v1` profile checks it: `check` compares it with the current plan, and `advance` moves
  it to a limited trial and then adoption only when the fixed criteria pass.
- `playbook`: conditional steps that use tools you are already allowed to use. It is not an executable
  plugin and is never published to Team Skills. It stays a candidate unless a qualified evaluator exists.

Your own assessment, tests you wrote, or several Agents agreeing do not count as an independent result.
Do not invent a receipt, set a method active, schedule experiments or change evaluation rules to pass.
Repeating the same query is not new evidence, and no improvement is a valid outcome. The owner's task and
its budget come first. The reference covers `select`, `evaluate` and the synthetic `artifact-contract-v1`
profile.

Before reusing an active method, check that its tools, account scope, object versions and knowledge
dependencies still hold. Retire a rejected method; never revive old approvals or replay actions whose
effects are unknown.

## Inspect and forget

`trace` shows, for a registered reference, where it was injected, cited or used to produce an artifact. It
does not prove that the note caused an outcome, and direct file reads or noCapture activity leave gaps.

Run `forget plan` and `forget apply` only when the owner explicitly asks for maintenance, and show the plan
first. Forgetting removes the note with its registered derivatives and counterexamples. It does not erase
native history, provider logs, external artifacts or backups, and it must not leave a copy under another
name. A correction may keep a counterexample; forgetting may not. `import` is also owner-initiated: plan
it for selected files and never scan HOME.
