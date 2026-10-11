# Claude API fallback allowance

This optional guard shares one dollar allowance across selected Claude API keys
and every client using them. Subscription OAuth and other providers are unchanged.
It accounts only for requests through this gateway, not direct API usage elsewhere.
It does not change Anthropic account credits or its paid-spending limit.

## Configuration

Add `budget-id` to the existing Claude fallback group (or its individual keys),
retaining its existing priority, models, and secrets. Group keys inherit the ID;
an explicit key ID overrides it. Use the same ID and state path for every key that
shares the allowance. Rotating secrets or generated auth IDs does not reset usage.

```yaml
api-keys:
  claude:
    - name: anthropic-credit-fallback
      priority: -10
      budget-id: anthropic-included-credit
      keys: [] # Retain the real existing keys here; do not replace them with [].

upstream:
  claude:
    api-budgets:
      anthropic-included-credit:
        state-path: /gateway-data/anthropic-included-credit.json
        limit-micro-usd: 100000000 # USD 100
        initial-used-micro-usd: 10000 # USD 0.01, applied only to a new ledger
        monthly-anchor: "2026-10-11T00:00:00"
        timezone: UTC
        allowed-betas:
          - interleaved-thinking-2025-05-14
          - fine-grained-tool-streaming-2025-05-14
        models:
          claude-opus-5-5:
            max-input-tokens: 1000000
            max-output-tokens: 128000
            input-micro-usd-per-million: 4000000
            output-micro-usd-per-million: 20000000
            cache-write-5m-micro-usd-per-million: 5000000
            cache-write-1h-micro-usd-per-million: 8000000
            cache-read-micro-usd-per-million: 200000
          claude-sonnet-5-5:
            max-input-tokens: 1000000
            max-output-tokens: 128000
            input-micro-usd-per-million: 2000000
            output-micro-usd-per-million: 10000000
            cache-write-5m-micro-usd-per-million: 2500000
            cache-write-1h-micro-usd-per-million: 4000000
            cache-read-micro-usd-per-million: 100000
          claude-haiku-5-5:
            max-input-tokens: 1000000
            max-output-tokens: 128000
            input-micro-usd-per-million: 500000
            output-micro-usd-per-million: 2500000
            cache-write-5m-micro-usd-per-million: 625000
            cache-write-1h-micro-usd-per-million: 1000000
            cache-read-micro-usd-per-million: 50000
```

These example prices and ceilings were read from the public
[pricing](https://platform.claude.com/docs/en/about-claude/pricing.md) and
[context window](https://platform.claude.com/docs/en/build-with-claude/context-windows.md)
documents on 2026-10-11. Verify them before activation and when provider pricing
changes. Haiku uses its higher, over-100K prompt tier for all requests, deliberately
overcharging shorter prompts in the local ledger. Rates are not built into the
binary. Unknown models fail closed. This example assumes **midnight UTC renewal**;
confirm the actual grant time/timezone before using it. The example's next boundary
is 2026-11-11, and every following calendar month on the 11th. Timezone defaults to
UTC; `monthly-anchor` is a local date/time without an offset. A day absent from a
month is clamped to that month's last day, preserving the original day afterward.

Create the parent directory on the existing durable gateway volume, writable only
by the gateway user. The guard uses an absolute path, private files, a persistent
`.lock` file, OS `flock`, atomic replacement, file fsync, and directory fsync.
Supported activation requires Unix and a local filesystem with those guarantees.
Use one active gateway process for ordinary hot reload. If processes overlap,
they must use identical allowance settings; drain all of them before changing
those settings across processes. File locking serializes their ledger writes,
but configuration reload coordination is local to a process.
Do not run replicas with separate copies of the ledger, use NFS, delete/replace the
lock file, change the ID/path to obtain a fresh ledger, or put it on temporary storage.
Back up the JSON state and lock together. A missing state with an existing lock,
corrupt state, or persistence failure blocks inference rather than initializing
another allowance. A first-write failure may leave only a lock; reconcile it before
reinitializing. A genuinely new directory/ID is an operator action, not a reset API.

## Admission and settlement

Before every protected upstream attempt, the guard durably charges:

```
model maximum input tokens * highest configured input/cache rate
+ final request max_tokens * configured output rate
```

Each category rounds upward to an integer microdollar. It reads the final business
payload after payload rules without changing it. It reserves the provider's hard
input ceiling because local tokenizers and upstream token-count estimates do not
prove an upper bound, especially with images, documents, tool schemas, and thinking.
Token counting remains free and does not consume a reservation. For example, an
Opus request with `max_tokens: 64000` reserves USD 9.28, then normally settles much
lower. At `128000`, its bound is USD 10.56. This limits concurrency and can reject
requests while a positive balance remains. Smaller `max_tokens` reduces the output
reservation; lowering the documented input ceiling to improve admission invalidates
the cost bound and must not be used as a tuning knob.

The guard settles synchronously from complete raw JSON usage or SSE
`message_start`/`message_delta` usage followed by `message_stop`, before translation
and before exposing the terminal response. It distinguishes 5-minute and 1-hour
cache creation, reads cached input separately, and never adds thinking twice.
Responses without cache TTL breakdown use the higher configured write rate.
Retries are separate reservations. Settlement is idempotent. Errors, cancellation,
missing usage, truncated streams, process crashes, and failed settlement writes keep
the full reservation charged. Pending reservations also carry across renewal.
Complete usage that cannot be priced within its bound additionally blocks the
allowance until the operator reconciles it; raising the cap or renewal does not
clear that block. No asynchronous usage queue or lagging provider report controls
admission.

The bound assumes the provider honors the configured model/context/output ceilings,
rates, and approved billing semantics. It is not a provider-enforced account-wide
USD 100 cap. Keep the separate provider spending limit as a second control. Prompt
prices are conservative estimates of dollars, not an invoice reconciliation feed.

Before payload rules, the executor defaults an omitted `service_tier` to
`standard_only` for protected keys. An explicit automatic/priority tier, or a user
rule filtering the standard-only field, is rejected at the final send boundary.
The guard never restores a filtered field. Ambiguous duplicate JSON keys are also
rejected before sending.

Standard first-party Messages inference supports text, images, documents, thinking,
client-executed custom tools, and the listed client-executed computer/bash/editor
tool versions. The guard rejects fast mode, automatic/priority tiers, regional
inference premiums, server tools, remote MCP, containers, compaction/extra sampling,
unknown tool versions, unknown top-level request features, and unapproved betas.
Do not approve a beta unless its billing is bounded by the configured rates and
single-sampling ceilings. In particular, do not approve server-side model fallback.
Ordinary Claude SDK requests need no beta allowlist additions; CLI fingerprint
opt-ins may need explicitly verified, nonbilling betas. New billable features
require implementation and tests before activation. The final payload is never
silently rewritten to remove these features.

Generic HTTP generation, batches, and other paid endpoints are disabled for
protected keys. Generic model reads and `messages/count_tokens` remain available.
Automatic redirects are disabled on protected sends, including token counts, to
avoid unreserved POST replays. Paid requests also disable the transport's body
factory so idempotency headers cannot trigger internal replays after an uncertain
outcome. All normal executor retries are charged independently. API-key requests to
third-party gateways are unsupported when budgeted, because their billing cannot
be inferred from Anthropic prices.

## Status and live adjustments

On Linux, v8.0.21 watches the config file itself. Save edits in place (preserving
its inode) for hot reload, or restart the gateway after replacing the file.
Reading the management config endpoint alone does not prove the executor reloaded:
verify the applied ledger settings on the next protected request.

The atomic JSON state contains no secrets. Inspect it locally without an API/UI:

```sh
jq '{id, cycle_start, next_renewal, used_micro_usd, limit_micro_usd,
     remaining_micro_usd: ([0, (.limit_micro_usd - .used_micro_usd)] | max),
     pending_count: (.pending | length), blocked_reason}' \
  /gateway-data/anthropic-included-credit.json
```

Used includes pending reservations. It can be higher than the real provider bill.
A budget rejection returns HTTP 429 with used, limit, remaining, the required
reservation, and next renewal. Invalid/missing prices/features reject before send;
storage or reconciliation failures block with HTTP 503. The normal scheduler may
still serve an available subscription credential.

Change `limit-micro-usd` and/or `monthly-anchor`/`timezone` in the ordinary config
and let its existing hot reload update executors. Requests still being prepared by
superseded executors fail before admission rather than restoring older settings.
The next admission persists the new limit and schedule without clearing usage.
Status shows last-applied settings
until that admission. Raising/lowering the limit does not reset spend or modify
already sent requests. Lowering below existing charges blocks further admission.
A schedule edit preserves all accrued spend and chooses the new schedule's next
future boundary. It does not reinterpret an earlier boundary as an immediate
renewal. If unchanged, renewal is applied on the first admission at/after its
boundary; settled spend resets, while pending charges and reconciliation blocks
remain. An older in-flight settlement does not restore older settings.

`initial-used-micro-usd` is used only when initializing the ledger. Editing it later
does not correct or reset existing usage. To reconcile uncertain charges:

1. Stop/drain this gateway and all processes sharing this ledger. Do not clear a
   reservation for an active or possibly still executing provider request.
2. Back up the state and its lock. Wait until provider reports cover every completed
   attempt, including retries and the provider's reporting lag. Separate direct
   API usage if reconciling only this gateway's allowance.
3. With independently verified cycle spend, edit `used_micro_usd` to an integer
   microdollar total. Remove only proven-completed entries from `pending`; retain
   uncertain entries and add their complete reservation amounts to the verified
   total. Ensure `used_micro_usd >= sum(pending.values())`. Preserve version, ID,
   schedule, cycle dates, and the lock file. Clear `blocked_reason` only after
   correcting the cause and conservatively reconciling any excess cost.
4. Write a temporary file in the same directory with mode 0600, fsync it, rename it
   over the state, and fsync the directory. Restart the gateway and check the state
   and next admission. Do not delete the ledger to reconcile it.

For the specified October 2026 activation, seed USD 0.01 (10000 microdollars), cap
USD 100, confirm the November 11 renewal instant, retain the OAuth-first/fallback
priority settings, and leave Anthropic's existing USD 1 paid-spending limit alone.
All devices, including Ferdinand, must use these protected fallback keys through
this single gateway volume to share the allowance.
