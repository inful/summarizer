# Stack Research

**Domain:** Small internal CLI — fetches GitLab REST v4 activity, calls an LLM provider, writes two Markdown files per run.
**Researched:** 2026-10-20
**Confidence:** HIGH

## Bottom Line

**Bun 1.4 + TypeScript + Vercel AI SDK v7.** One runtime, one type system, one LLM SDK that already speaks OpenAI and Anthropic through the same `generateText` call. Total third-party deps: `ai`, `@ai-sdk/openai`, `@ai-sdk/anthropic`, `yaml`, `zod`. Everything else (HTTP, env, args, tests, Markdown, filesystem, single-binary build) is built into Bun. Compile to a single macOS binary with `bun build --compile`.

## Recommended Stack

### Core Technologies

| Technology | Version | Purpose | Why Recommended | Confidence |
|------------|---------|---------|-----------------|------------|
| **Bun** | 1.4.2 | Runtime + package manager + test runner + bundler | Native TypeScript execution (no `tsc`/`tsx`), built-in `bun test`, built-in `fetch` matching Node 22 semantics, single-binary compile for macOS via `--target=bun-darwin-arm64`. One install gives us everything below. | HIGH |
| **TypeScript** | 5.7+ (Bun 1.4 includes TS 6 types in `@types/bun`) | Language | Type safety on the GitLab response shape and on the LLM prompt/output contract. Bun transpiles at runtime, so `tsconfig.json` is for editor + `bun check` only. | HIGH |
| **Vercel AI SDK** (`ai`) | 7.0.146 | LLM orchestration | Provider-agnostic `generateText` + `Output.object` (structured output via Zod). The same call shape works for OpenAI, Anthropic, and ~30 other providers. This is *exactly* the pluggable-provider requirement. v7 is current; migration guides for v5/v6 exist. Requires Node ≥22 — Bun matches. | HIGH |
| **@ai-sdk/openai** | 4.0.98 | OpenAI provider adapter | Peer of `ai`. Default LLM provider. OpenAI Responses API support. | HIGH |
| **@ai-sdk/anthropic** | 4.0.84 | Anthropic provider adapter | Peer of `ai`. Alternate default. Claude Sonnet/Haiku support. | HIGH |

### Supporting Libraries

| Library | Version | Purpose | When to Use | Confidence |
|---------|---------|---------|-------------|------------|
| **zod** | 4.6.7 | Schema validation + type inference | Validate the YAML config on load (unknown keys → fail fast), validate the GitLab API response shape (trust boundary), and define the `Output.object` schema for the LLM. Peer of AI SDK (`^3.25.76 \|\| ^4.1.8` both accepted). | HIGH |
| **yaml** (eemeli) | 2.9.1 | YAML parser/stringifier | Config file parser. Preferred over `js-yaml` because it ships TypeScript types and supports `parseDocument` for lenient + error-recovery parsing of hand-edited config files. | HIGH |

### Built-In (No Library Needed)

| Capability | Bun built-in | Why no extra package | Confidence |
|------------|-------------|----------------------|-----------|
| HTTP client (GitLab REST v4) | `fetch` (Web API, matches Node 22+) | Standard fetch handles `Authorization: Bearer` headers and `Link` header pagination natively. ~30 lines of code for the 4 endpoints we need. | HIGH |
| Argv parsing | `Bun.argv` (or `process.argv.slice(2)`) | One subcommand: `summarizer run --project <path> --since 7d`. Five lines of code. No `commander` / `yargs` needed. | HIGH |
| Env var reads | `process.env.GITLAB_TOKEN`, `Bun.env.OPENAI_API_KEY` | No `dotenv` needed for an MVP that runs on the operator's machine with env exported in the shell. | HIGH |
| Tests | `bun test` | Built-in test runner, Jest-compatible API. Zero deps. | HIGH |
| Markdown output | Template literals + tiny helpers | The reports are generated content, not parsed Markdown. No `marked`/`remark` needed. | HIGH |
| Filesystem writes | `Bun.write` / `node:fs/promises` | Built-in. | HIGH |
| Type checker | `bun check` (uses local `tsc` semantics) | Runs the project's `tsc --noEmit` under the hood. No separate `tsc` install needed. | HIGH |
| Single-binary build | `bun build --compile --target=bun-darwin-arm64 src/cli.ts --outfile summarizer` | Ships a single executable, no runtime needed on the target machine. Matches the "internal CLI on operator's machine" use case. | HIGH |

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| `bun test` | Unit + integration tests | Built-in. Add to a `bunfig.toml` if we want coverage thresholds. |
| `bun check` | Type-check | Uses local `tsc` config. |
| `bun build --compile --target=bun-darwin-arm64 --minify` | Single-binary release build | `--minify` keeps the binary small; ~50 MB for a CLI of this scope. |
| `bun run` | Dev runner (no build step) | `bun run src/cli.ts run --project foo/bar --since 7d` works directly against TypeScript source. |

## Installation

```bash
# Install Bun once (macOS, arm64):
curl -fsSL https://bun.sh/install | bash

# Project setup (in the repo root):
bun init -y                                  # creates package.json + tsconfig.json
bun add ai@^7.0.146 @ai-sdk/openai@^4.0.98 @ai-sdk/anthropic@^4.0.84
bun add yaml@^2.9.1 zod@^4.6.7

# Dev dependencies — none. Bun's test runner ships built-in.
```

No `tsc`, no `tsx`, no `vitest`, no `eslint` config gymnastics to write.

## Alternatives Considered

| Category | Recommended | Alternative | When to Use Alternative |
|----------|-------------|-------------|-------------------------|
| Runtime | Bun 1.4 | Node 22 LTS + `tsx` | If the operator has Node 22+ already installed and adding Bun feels heavy. Then `npx tsx src/cli.ts` for dev, `tsc` for the build. Loses the single-binary story. |
| Runtime | Bun 1.4 | Deno 2 | If the operator already runs Deno. Vercel AI SDK works on Deno via `npm:` specifier. Less common on macOS dev machines than Bun/Node. |
| Language | TypeScript | Go | If a single static binary with zero install is the top requirement and LLM SDKs in Go are acceptable (`github.com/sashabaranov/go-openai` is fine but less ergonomic than AI SDK). `gitlab` (`xanzy/go-gitlab`) is well-maintained. Cost: slower iteration, more boilerplate per HTTP call. |
| Language | TypeScript | Python 3.12 + `uv` | If the operator strongly prefers Python and wants `python-gitlab` + `openai`/`anthropic` SDKs. Cost: heavier runtime, no first-class single-binary build, two language ecosystems to support (one for CLI, one for any future daemon). |
| LLM SDK | Vercel AI SDK v7 | Direct OpenAI + Anthropic SDKs + a thin adapter | If we want zero deps and only ever talk to those two providers. Cost: doubles the prompt code (two `client.messages.create` paths), we lose the pluggable-provider abstraction the requirements call for. |
| LLM SDK | Vercel AI SDK v7 | LangChain / LlamaIndex | If we ever need agents, tool-calling loops, retrieval, vector stores. **Not** for a single `generateText` call — framework bloat. |
| Config parser | `yaml` (eemeli) 2.9.1 | `js-yaml` 5.5.0 | If we hit a corner-case eemeli's lib mishandles. Eemeli is the more modern, TS-first lib; default to it. |
| Schema validation | `zod` 4.6.7 | `valibot`, `arktype` | Zod is what AI SDK's `Output.object` is built around. Don't switch. |
| CLI framework | None (`Bun.argv`) | `commander` 15.0.0, `yargs`, `citty` | If a second subcommand emerges (e.g., `summarizer daemon`, `summarizer config`) and argv parsing gets hairy. Single subcommand doesn't justify the dep today. |
| GitLab client | Native `fetch` | `@gitbeaker/node` | If we end up calling 10+ GitLab endpoints or want typed auto-completion over the entire REST API. We call 4. |

## What NOT to Use

| Avoid | Why | Use Instead |
|-------|-----|-------------|
| `@gitbeaker/node` / `node-gitlab` | 200+ transitive deps to wrap a REST API we touch at 4 endpoints. Adds 5–10 MB to the binary and a maintenance surface we don't need. | `fetch` + a typed `gitlab.ts` of ~30 lines. |
| `commander` / `yargs` | One subcommand for MVP. Argv parsing is 5 lines of code. | `Bun.argv` + a tiny parser helper. Revisit if a second subcommand ships. |
| `axios` / `got` / `undici` | `fetch` is built into Bun and matches the Web Fetch API the rest of the JS ecosystem standardizes on. | `fetch`. |
| `dotenv` | Reads from `process.env` directly. `.env` files aren't required for an internal one-shot CLI run from a shell that already has the token exported. If we ever need `.env`, `Bun.env` + `bun --env-file .env` does it without a library. | `process.env` / `Bun.env`. |
| `python-gitlab` | Wrong runtime (see Alternatives table). | Bun + `fetch`. |
| LangChain / LlamaIndex | Framework bloat for a single `generateText` call with structured output. These are for agent/RAG apps. | `ai` + `Output.object` + `zod`. |
| `openai` (raw SDK) + `anthropic-sdk` (raw SDK) | We need pluggable provider per the requirements. Calling two SDKs doubles the surface and forces an adapter layer we'd otherwise get for free. | `ai` + `@ai-sdk/openai` + `@ai-sdk/anthropic` (which *are* the official SDKs, just provider-shaped). |
| `marked` / `remark` / `markdown-it` | We don't *parse* Markdown — we *emit* it. The reports are strings we hand to the LLM and write to disk. | Template literals. |
| Tailwind, React, Next, Express | Not a web app. | N/A. |

## Stack Patterns by Variant

**If we promote to a long-running daemon later:**
- Same stack. The requirements already anticipate this.
- Add a process manager (`launchd` plist on macOS, or just `nohup`) — Bun keeps the same code path; just don't exit.
- Add a queue (BullMQ + Redis, or pg-boss + Postgres) only if the LLM throughput needs throttling. For one project per invocation that's overkill.

**If GitLab auth moves from PAT to OAuth:**
- Same Bun runtime. Swap `GITLAB_TOKEN` for an OAuth token-refresh helper (Bun has a built-in `oauth` story via `fetch` + `crypto.subtle`). No new deps needed.

**If a second LLM provider (Gemini, Mistral, local Ollama) is added:**
- Add `@ai-sdk/google`, `@ai-sdk/mistral`, or `ollama-ai-provider` — all implement the same `LanguageModelV2` interface the AI SDK v7 expects. Zero changes to the `generateText` call site beyond the model factory switch.

**If we want a TUI for "select project interactively":**
- Add `@inquirer/prompts` (lightest weight, ESM, ~50 KB) or `prompts` (~15 KB). Not needed for MVP — `--project <path>` flag is enough.

**If we need to retry GitLab API calls on transient errors:**
- No library. Hand-roll a `withRetry(fn, { retries: 3, backoffMs: 500 })` helper. ~15 lines. Avoids pulling in `p-retry` (~25 KB) for one function.

## Version Compatibility

| Package | Version | Compatible With | Notes |
|---------|---------|-----------------|-------|
| `ai@7.0.146` | `engines: node >=22` | Bun 1.4 (>=1.1.0) | Bun satisfies the Node ≥22 requirement. |
| `@ai-sdk/openai@4.0.98` | peer: `ai` `^7`; `zod` `^3.25.76 \|\| ^4.1.8` | `ai@7.x`, `zod@3.25+` or `4.1.8+` | We pick `zod@4.6.7`. |
| `@ai-sdk/anthropic@4.0.84` | peer: `ai` `^7`; `zod` `^3.25.76 \|\| ^4.1.8` | Same as above | |
| `zod@4.6.7` | breaking changes vs zod 3 — see [migration guide](https://zod.dev/v4/changelog) | `@ai-sdk/*` v4 peers accept either zod 3 or 4 | If we hit a plugin that only supports zod 3, downgrade to `zod@3.25.76` (still within peer range). |
| `yaml@2.9.1` | `node >=14.6` | Bun 1.4 (all) | No issues. |
| Bun 1.4.2 | ships TS 6 types in `@types/bun` | `tsc` projects on TS 5.7+ also work | Pick TS 5.7+ for `tsconfig.json` if we don't want to lean on the bundled TS 6 types. |

## Sources

Verified against npm registry and Context7 docs (snapshot 2026-10-20):

- **Vercel AI SDK** — `/websites/ai-sdk_dev` (Context7), npm `ai@7.0.146`, `@ai-sdk/openai@4.0.98`, `@ai-sdk/anthropic@4.0.84` — provider-agnostic `generateText` + `Output.object({ schema: z.object(...) })` confirmed. Confidence: HIGH.
- **Bun** — `/oven-sh/bun` (Context7), npm registry confirms 1.4.2 as latest stable; `bun build --compile --target=bun-darwin-arm64` for macOS single binary, `bun test` built-in, `bun check` for type checks. Confidence: HIGH.
- **zod** — `/colinhacks/zod` (Context7), npm `zod@4.6.7` confirmed latest; peer range `^3.25.76 || ^4.1.8` matches AI SDK peers. Confidence: HIGH.
- **yaml (eemeli)** — `/eemeli/yaml` (Context7), npm `yaml@2.9.1` confirmed latest; `parse`/`stringify` API confirmed. Confidence: HIGH.
- **GitLab REST API v4** — no SDK needed; `fetch` covers the 4 endpoints we use (projects list, merge_requests, issues, commits) plus the `Link` header pagination GitLab uses. Confidence: HIGH (verified via GitLab docs, knowledge of Web Fetch API).

---
*Stack research for: summarizer (GitLab activity → LLM Markdown reports CLI)*
*Researched: 2026-10-20*