# Synapse Compliance Guide

This document is for the person who has to answer an auditor's question about what an AI agent did, why, and with what context. It maps Synapse's signed memory-compilation ledger to the transparency obligations of the EU AI Act — Article 50 of Regulation (EU) 2024/1689 — documents how to run an incident investigation and pull a compliance report, and states the limits plainly.

Synapse is traceability infrastructure. It logs and cryptographically signs *what context an AI agent was given and how each memory was scored*. It is not an AI system provider's transparency notice, it does not label model output, and nothing here is legal advice — see [Full disclaimer](#full-disclaimer).

**Contents**

- [Before you start: the tier gate](#before-you-start-the-tier-gate)
- [Signed ledger to EU AI Act Article 50 mapping](#signed-ledger-to-eu-ai-act-article-50-mapping)
- [Digital Omnibus on AI](#digital-omnibus-on-ai)
- [Running an incident investigation](#running-an-incident-investigation)
- [Downloading the PDF compliance report](#downloading-the-pdf-compliance-report)
- [Memory supersession is one turn late](#memory-supersession-is-one-turn-late)
- [Retention policy recommendations](#retention-policy-recommendations)
- [Chain break recovery](#chain-break-recovery)
- [Full disclaimer](#full-disclaimer)

## Before you start: the tier gate

All three compliance surfaces — `/v2/compliance/audit`, `/v2/compliance/chain-integrity`, and `/v2/compliance/report` — are gated on the caller's verified `compliance_tier` JWT claim being `enterprise`. The signed claim decides, not the registry row, and no request parameter can name a tier or another tenant's chain.

**The gap to plan around:** `POST /v2/tenants` currently mints `team` for every tenant it creates, whatever its `plan` is set to. A tenant provisioned through the public API therefore receives

```
{"error":"compliance_tier_required","upgrade_url":"https://synapse.ai/enterprise"}   HTTP 403
```

from all three surfaces, and this release has no HTTP route that mints an enterprise tier. Until that exists, an enterprise-tier token has to be issued through the tenant provisioner directly (`tenant.NewProvisioner` with `ComplianceTier: "enterprise"`) — which is how the project's own tests provision the tenants these surfaces are verified against. This is a product gap, not a policy: it is recorded as an open item in the project's phase notes.

Two more gate behaviours worth knowing before writing tooling:

- **A refusal is recorded.** When the plane has an auditor wired, a 403 is preceded by a row in `synapse_global.compliance_access_log` naming the endpoint, the caller's window and paging, a hashed remote address, and the response code. A refused read of a compliance surface is visible in the tenant's own access log.
- **The gate runs before parameter validation**, so a non-enterprise caller cannot use response codes to probe which query shapes are valid.

## Signed ledger to EU AI Act Article 50 mapping

Article 50 of Regulation (EU) 2024/1689 sets transparency obligations that fall on the *provider* or *deployer* of an AI system. Synapse is neither — it is infrastructure that sits between an agent and a model — so it cannot discharge any of them. What it can do is produce the signed, tamper-evident record a deployer needs in order to evidence that a given interaction happened, on what context, and in what order, and to prove later that the record was not altered. The paragraphs below state, obligation by obligation, what the ledger contributes and where its contribution stops.

**Article 50(1) — informing people that they are interacting with an AI system.** The provider of an AI system intended to interact directly with natural persons must ensure those persons are informed they are interacting with an AI, unless that is obvious to a reasonably well-informed person. Synapse issues no such notice; it emits no user-facing text at all. The ledger's contribution is evidentiary: each compilation is signed with a `trace_id`, a UTC timestamp, the detected intent and its confidence, the memories that reached the context, the memories that were excluded with their `exclusion_reason`, and the token budget applied. A reviewer asking "was this interaction AI-generated, and on what basis?" can be answered from the chain, and the answer can be shown not to have been edited since.

**Article 50(2) — machine-readable marking of synthetic output.** Providers of AI systems that generate synthetic audio, image, video, or text must ensure the output is marked in a machine-readable format and detectable as artificially generated or manipulated. Synapse sits in front of the model and does not modify or label the response — it forwards the upstream body, forced non-streaming so the reply can be captured. What it adds is input-side provenance that neither a vendor nor a watermark can supply: the exact memory set a generation was conditioned on, signed. If your marking programme needs to answer "which context produced this output?", the ledger's `trace_id` plus the entry's `hash_value` is a join key; the marking obligation itself is untouched.

**Article 50(3) — emotion recognition and biometric categorisation.** Deployers of emotion recognition or biometric categorisation systems must inform the people exposed to them, and handle the personal data involved under the GDPR. Synapse performs neither function, and its memory types carry no biometric category — they are `decision`, `error`, `fact`, `context`, and `preference`. The ledger's contribution is a control rather than a notice: the memory-type breakdown in the report and the per-memory types in each trace let a reviewer confirm which categories of memory actually reached contexts, and memory content is sanitized before storage. If a category that should never have been used reached a context, that is visible and it is signed.

**Article 50(4) — deep fakes.** Deployers of AI systems that generate or manipulate image, audio, or video content constituting a deep fake must disclose that the content is artificially generated or manipulated. Synapse is text-context infrastructure and generates no media. Its relevance is the same join as 50(2): a signed entry ties a conversation, an agent identity, a memory set, and a timestamp together, which is what lets a deployer reconstruct after the fact that a piece of media came out of an AI-assisted workflow. The disclosure remains the deployer's to make.

**Article 50(5) — text published to inform the public on matters of public interest.** Deployers who publish AI-generated or manipulated text to inform the public on matters of public interest must disclose that the text was generated or manipulated. This is the obligation closest to the data Synapse actually holds: the trace records memory content previews, memory types, per-factor scores, the intent classification, and the conflict and supersession state of every memory involved, all under a signed hash chain. A later reviewer can therefore see which stored facts and decisions shaped a body of text and can verify that the record was not altered afterwards. It evidences the process; it does not produce the public disclosure, and it does not establish whether a given text qualified as a matter of public interest.

**Article 50(6) and 50(7).** 50(6) carves out systems that only assist with standard editing, that do not substantially alter the input data, or that are authorised by law to detect, prevent, or investigate criminal offences. Those carve-outs turn on what your system does, not on what Synapse records — but the record is kept either way, which is what a deployer wants if a carve-out is ever questioned. 50(7) directs the AI Office to encourage codes of practice for detecting and labelling AI-generated content; the per-compilation provenance the ledger carries (which context, from which agent, when, signed) is the shape of evidence such a code would expect a deployer to be able to produce.

**Scope.** Article 50 is the transparency obligation most relevant to a memory plane. A high-risk classification additionally brings record-keeping obligations (Articles 12 and 19) and post-market monitoring duties; those depend on your own classification of the system and are outside this document, though the same ledger is the artifact they would draw on. See [Digital Omnibus on AI](#digital-omnibus-on-ai) for the current dates.

## Digital Omnibus on AI

The Digital Omnibus on AI — Regulation (EU) 2026/1744, in force 27 July 2026 — amended the AI Act's application timeline. For the purposes of this document, what it changed:

| Item | Original AI Act date | After the Omnibus |
|---|---|---|
| Article 50 transparency obligations | 2 August 2026 | **2 August 2026 — unchanged** |
| Annex III high-risk obligations | 2 August 2026 | **2 December 2027** |
| Annex I (product-embedded) high-risk obligations | 2 August 2027 | deferred beyond the Annex III date |

The practical reading: the deadline for **high-risk** conformity work moved out by roughly sixteen months, and the **transparency** obligations did not move at all. A deployment that is live today therefore has Article 50 duties now, and a deployment building toward a high-risk classification has more calendar for the heavier conformity work. The ledger matters for both — transparency evidence now, Article 12-style logging by the time the high-risk clock runs out.

Dates in this area are regulatory and have moved before. Treat this table as the state of Regulation (EU) 2026/1744 as published, and confirm the current text against the Official Journal before relying on a date for a filing.

## Running an incident investigation

`GET /v2/compliance/audit` pages one tenant's own signed ledger entries, newest first, over a window the caller chooses. It is the endpoint to reach for when the question is "what did this agent actually get, between these two times?". Prerequisites: an enterprise-tier token for the tenant (see [Before you start: the tier gate](#before-you-start-the-tier-gate)), a plane reachable by default on loopback `127.0.0.1:9090`, and ledger entries in the window.

**1. Establish the window.** Both bounds are RFC 3339 and optional; an omitted bound means unbounded on that side. Use UTC. An inverted window (`until` before `since`) is a valid question whose answer is an empty page, not an error.

```bash
export T='2026-09-20T00:00:00Z'   # incident start, UTC
export U='2026-09-21T00:00:00Z'   # incident end, UTC
```

**2. Page the ledger.** `limit` defaults to 50 and is capped at 200 — a page carries whole trace manifests, so the cap is what keeps one request from serializing an entire chain. `offset` advances through the window.

```bash
curl -sS "http://127.0.0.1:9090/v2/compliance/audit?since=$T&until=$U&limit=50&offset=0" \
  -H "Authorization: Bearer $SYNAPSE_ENTERPRISE_JWT"
```

**3. Read one entry.** The response is `{"data": [...], "total": N, "limit": N, "offset": N}`; `total` is how many entries the window holds, so you know when you have read them all. Each element of `data` looks like this:

```json
{
  "id": "9bec637b-4d1e-4e2b-9a0f-6c1d2e3f4a5b",
  "tenant_id": "b63b1d9d-898b-49ab-906e-64ed3edfa427",
  "request_id": "0912b375-cbac-5058-8b2d-6d3cf774d527",
  "trace": {
    "timestamp": "2026-09-20T13:13:16.687888Z",
    "detected_intent": "debug",
    "intent_confidence": 0.8,
    "candidates_retrieved": 12,
    "candidates_after_dedup": 9,
    "memories_compiled": 4,
    "tokens_used": 812,
    "token_budget": 3000,
    "compile_duration_ms": 47,
    "memories": [
      {
        "id": "req-1790071552527909122",
        "memory_type": "error",
        "content_preview": "the synapse_compile tool returns invalid_params …",
        "score_semantic": 0.71,
        "score_recency": 0.99,
        "score_importance": 0.9,
        "score_task_alignment": 0.67,
        "score_total": 0.83,
        "included": true,
        "agent_id": "edge-01",
        "cross_agent": false,
        "conflict_status": "none"
      }
    ]
  },
  "prev_hash": "aeebad4a796f…",
  "hash_value": "f5e10eca7123…",
  "created_at": "2026-09-20T13:13:16.687888Z"
}
```

The `trace` is the decision record: what was retrieved, what was dropped as a duplicate, what was compiled, which memories were excluded and why (`exclusion_reason`), each memory's four factor scores, and the provenance fields (`agent_id`, `cross_agent`, `conflict_status`, `conflict_with_id`) that say whose memory it was and whether it had been contradicted.

**4. Prove the window has not been altered.** Walk the chain, which covers the tenant's *whole* chain rather than just the window:

```bash
curl -sS http://127.0.0.1:9090/v2/compliance/chain-integrity \
  -H "Authorization: Bearer $SYNAPSE_ENTERPRISE_JWT"
# {"entries_checked":128,"chain_valid":true,"first_break_id":"","first_break_at":"0001-01-01T00:00:00Z"}
```

A broken chain is still a **200** with `"chain_valid": false`. That is deliberate: an error status would collapse "your record no longer verifies" into "the check could not run", which is exactly the distinction the endpoint exists to draw. See [Chain break recovery](#chain-break-recovery) when `chain_valid` is false.

**5. Line the traces up with metering.** Volume and mean reduction for the same window come from `synapse_global.usage_events` and are summarised in the report as `summary.total_compilations`, `summary.total_memories_used`, and `summary.avg_reduction_pct`. A window that predates metering falls back to the signed traces' own values, which are `0` — and the response does not say which of the two it used.

**6. Check who else has read this history.** Every answer on this endpoint, refusals included, is preceded by a row in `synapse_global.compliance_access_log`: endpoint, window and paging, a hashed remote address, and the response code. The address column is `hex(sha256(remote address))` — a stable pseudonym, not an anonymisation. It answers "did one client page this history twenty times?" without the table ever holding a raw address.

**7. Preserve.** The ledger is append-only — the plane writes it through an INSERT-only role and has no update or delete path — so the evidence you have just read cannot be edited later through Synapse. Export the JSON pages and, if the auditor wants a document, the PDF below. Keep the JSON as well as the PDF: a PDF is a rendering of the JSON, and a reviewer working from the JSON can re-run steps 4 and 5 themselves.

**Errors to expect:** `400` with `invalid_since`, `invalid_until`, `invalid_limit`, or `invalid_offset` for malformed parameters; `401` for a token that does not verify; `403` for the tier gate; `500` when the read itself could not run — the underlying error is logged server-side and never reflected, because a database error can quote the connection target.

## Downloading the PDF compliance report

`GET /v2/compliance/report` returns the same document as JSON (`format=json`, the default) or as a print-ready A4 PDF (`format=pdf`), over the same optional window. One curl command:

```bash
curl -sS -o compliance-report.pdf \
  "http://127.0.0.1:9090/v2/compliance/report?format=pdf&since=2026-01-01T00:00:00Z" \
  -H "Authorization: Bearer $SYNAPSE_ENTERPRISE_JWT"
```

Omit `since`/`until` to report on the whole ledger rather than a window — a report that silently defaulted to the last 30 days would answer a different question than the one printed on its own header.

Both formats carry the same fields, in sections: `header` (tenant id, the period as requested, generation time, plane version), `summary` (`total_compilations`, `total_memories_used`, `avg_reduction_pct`), `memory_type_breakdown`, `global_brain` (`cross_agent_retrievals`, `unique_contributing_agents`), `conflict_resolution` (`contradictions_detected`), `supersession` (`memories_superseded`), `chain_integrity` (`valid`, `last_verified_at`, `entries_in_period`), and `article_50_statement`.

Two operational facts for PDF:

- **A renderer is required and is resolved from `PATH` at request time**: `wkhtmltopdf`, or the Chromium family (`chromium`, `chromium-browser`, `google-chrome`, `google-chrome-stable`) run headless with `--print-to-pdf`. With none installed, the endpoint answers `503 {"error":"pdf_tool_unavailable","message":"install wkhtmltopdf to enable PDF reports"}` — and `format=json` still works, so the JSON report is never blocked by a missing renderer.
- **The HTML template is configuration** (`report-template`, default `ui/plane/compliance-report.html`, relative to the plane's working directory). A plane that never serves a PDF boots without the file; a PDF request against a missing template is a logged `500` rather than a startup failure.

`format` is validated to exactly `json` or `pdf` before anything else happens — `PDF`, `xml`, or a value carrying shell metacharacters is a `400`, and the renderer is executed directly rather than through a shell.

The chain verdict inside the report covers the tenant's **whole** chain, not just the report's window (`entries_in_period` is the window's share of it). A report that certified only the rows it happened to read would certify nothing.

## Memory supersession is one turn late

Synapse marks a memory as superseded when a newer memory in the same session, of the same eligible type, lands inside a similarity band *and* carries an explicit change signal ("instead of", "switched to", "no longer"). The older memory is kept — supersession is a marker, not a delete — and the scorer demotes it: the trace carries `conflict_status: "superseded_candidate"` with `conflict_with_id` naming the memory that supersedes it, and the demotion lands on `score_total`.

**The timing property:** the write that marks the older memory happens on the write path, and the filtering that removes a superseded memory from consideration runs when the candidate pool is assembled. A compilation already in flight when the newer memory was written can therefore still include the superseded memory — so it may appear in **one additional compilation** before filtering takes effect.

**What that means for an audit:**

- **The compiled context is correct for the state it was compiled against.** A trace that includes the superseded memory is a faithful record of what the agent was given at that moment. It is not a corrupt record and it is not a failed filter.
- **The ordering is recoverable, so the finding is falsifiable.** Compare the compilation's `created_at` with the supersession write's own ledger entry. If the compilation precedes it, the older memory's presence is the expected ordering; if it follows it, that is worth raising.
- **The older memory appears demoted, not equal.** Its `score_total` carries the conflict penalty, so a single trace entry shows both the smaller number and the reason for it (`conflict_status`, `conflict_with_id`).
- **It is not a chain or integrity problem.** `chain_valid: true` and a one-turn-late supersession are entirely consistent. Nothing about the record was altered; the chain says so.
- **Nothing is hidden.** The superseded memory stays readable, which is what makes the sequencing above demonstrable in the first place.

**For the report:** supersession appears as `supersession.memories_superseded`, a count of distinct memories replaced in the period, counted once each however many traces they appear in. It is not an error count, and a non-zero value is normal operation — it means the write path marked replacements as intended.

For the record layout behind this behaviour, see `internal/trace/trace.go` (`TraceMemory.Included`, `ExclusionReason`, `SupersededBy`, `ConflictStatus`, `ConflictWithID`).

## Retention policy recommendations

`ledger-retention-days` in `synapse-plane.yaml` (default `365`) is the window your reporting and archival tooling should treat as online history.

| Sector | Recommended window | Rationale |
|---|---|---|
| General | **365 days** | The default: covers an annual audit cycle and the retrospective window most teams work to |
| Financial services | **2555 days (7 years)** | Matches the record-retention horizon that applies to financial record-keeping and supervisory requests |
| Healthcare | **2555 days (7 years)** | Matches the typical clinical-record retention horizon in most EU member states |

Two properties of the store to keep in mind when you write the policy:

- **The ledger is append-only and the plane never deletes a row.** It is written through an INSERT-only role, and the product has no update or delete path. `ledger-retention-days` therefore declares the window you *report over and archive against*; it does not expire data. A policy that must remove data has to rely on normal database administration and your own archive, not on the plane forgetting.
- **Archival is the mechanism for "longer than the window".** Export the JSON pages (`/v2/compliance/audit`) or a PDF, and keep the export verifiable: a page of signed entries carries `prev_hash` and `hash_value` per row, so the chain can be re-derived against the plane's own `entries_checked` count while the rows are still online. Keep the JSON, not only the PDF — a PDF is a rendering.

## Chain break recovery

A broken chain is reported, never hidden. `GET /v2/compliance/chain-integrity` (or its Phase 17 alias `GET /v2/ledger/verify`) walks the tenant's chain and answers:

```json
{
  "entries_checked": 5,
  "chain_valid": false,
  "first_break_id": "6f0a1c22-…",
  "first_break_at": "2026-09-20T13:14:02.113255Z"
}
```

`entries_checked` counts the breaking entry as examined — a break at the fifth row of a ten-row chain reports 5, because once one row is in doubt every later link is. `first_break_id` is the earliest entry that failed and `first_break_at` its timestamp. The response is a `200`: the check succeeded, and the finding is `chain_valid`.

**Procedure**

1. **Record the evidence first.** Save the exact response body and note the wall-clock time you fetched it.
2. **Do not attempt a repair by editing rows.** The table is append-only by design and by role; an edit is either rejected or is precisely the tampering the chain exists to reveal. There is no supported repair path, and building one ad hoc would destroy the property you are investigating.
3. **Preserve the database.** Take a snapshot (filesystem or `pg_dump`) of the plane's Postgres before further investigation — the running plane keeps appending past the break, and a snapshot pins the artifact.
4. **Pin down the window.** Re-read the entries around `first_break_id` with `/v2/compliance/audit` over a window spanning it, and export them.
5. **Contact support with the `first_break_id`.** The first_break_id from `/v2/compliance/chain-integrity` is the identifier that makes the report actionable; include it plus `first_break_at`, `entries_checked`, the tenant slug, the plane version, and whether anything else touched the database (restores, migrations, manual statements) in that window.
6. **Keep the two failures apart.** A `403` is the tier gate, `401` an unverifiable token, `500` a check that could not run at all, and `503` an unavailable PDF renderer (the report endpoint, not this one). None of those is a chain finding, and none should be reported as one.

**One limitation to state in any report that rests on this:** in this release the chain head is not anchored outside the database, so from the plane alone "the whole chain was deleted" and "nothing was ever appended" are not distinguishable. If that distinction matters for your audit, keep your own copy of the exported pages (see [Retention policy recommendations](#retention-policy-recommendations)). This limitation is recorded in the project's phase notes rather than left implicit.

## Full disclaimer

The following statement is carried into every compliance report, JSON and PDF, by the plane itself. It is the constant `plane.Article50Statement` in the source, reproduced here verbatim:

> This report is generated by Synapse Context Compiler, which provides cryptographically signed, tamper-evident audit trails of all memory compilation decisions made by AI agents in this organisation. Each compilation event is logged with its full decision trace, memory provenance, scoring rationale, and chain integrity verification. DISCLAIMER: This report provides technical traceability infrastructure. It does not constitute legal advice or guarantee regulatory compliance with Regulation (EU) 2024/1689 or any other instrument. Consult qualified legal counsel regarding your organisation's specific obligations under the EU AI Act.

## Related

- [README — EU AI Act compliance](README.md#eu-ai-act-compliance)
- [README — Global Brain setup](README.md#global-brain-setup)
- [README — MCP setup](README.md#mcp-setup)
- [README — Known limitations](README.md#known-limitations)
- `synapse-plane.yaml.example` — `ledger-retention-days`, `report-template`
- `deploy/` — the compose stack these endpoints run on