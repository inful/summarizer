# Stack Research

**Domain:** Small internal CLI — fetches GitLab REST v4 activity, calls an LLM provider, writes two Markdown files per run.
**Researched:** 2026-10-20
**Stack:** Go (operator-specified; supersedes the initial Bun + TypeScript draft)

## Bottom Line

**Go (1.23+) with stdlib everywhere it goes, plus exactly one third-party dep: `gopkg.in/yaml.v3`.** Both GitLab and the LLM providers are plain HTTP, so no SDK needed. `net/http` covers all three external services. CLI parsing via the stdlib `flag` package — subcommands (`run`, `--help`, `--version`) are a few lines of `flag.NewFlagSet`. Config is YAML validated by hand against the schema. Single static binary via `go build` — no runtime, no cgo, no dependency on the host beyond libc.

## Recommended Stack

### Core Technologies

| Technology | Version | Purpose | Why Recommended | Confidence |
|------------|---------|---------|-----------------|------------|
| **Go** | 1.23+ (operator pins 1.24+) | Language + toolchain | Single static binary via `go build`. Stdlib covers HTTP, flags, env, FS, JSON, tests. The operator's existing Go toolchain (Flux projects, registered `tokensave` projects) means zero new infra. | HIGH |
| **`net/http`** | stdlib | HTTP client + server | GitLab REST v4 and the OpenAI / Anthropic chat-completion endpoints are simple POSTs with bearer-token auth and JSON bodies. `http.Client` with a 30 s timeout is enough. `Link`-header pagination for GitLab is ~30 lines. | HIGH |
| **`flag`** | stdlib | CLI parsing | Two subcommands (`run`, `version`) and a handful of flags. No `cobra` needed at this scope. `flag.NewFlagSet` lets each subcommand own its own flag set; `flag.ContinueOnError` plus a manual `Usage` string gives clean `--help` output. | HIGH |
| **`os`** / **`io`** / **`path/filepath`** | stdlib | Filesystem, env, paths | `os.ReadFile`, `os.WriteFile` (with tmp + rename for atomic write), `os.LookupEnv` for `GITLAB_TOKEN` and LLM keys. | HIGH |

### Supporting Libraries

| Library | Version | Purpose | When to Use | Confidence |
|---------|---------|---------|-------------|------------|
| **`gopkg.in/yaml.v3`** | v3.0.1 | YAML parser | Config file parser. `Unmarshal` into a typed struct; reject unknown keys. No hand-rolled YAML. | HIGH |
| **`embed`** | stdlib (since Go 1.16) | Prompt templates | Embed prompt text files at compile time via `//go:embed prompts/*.md`. Editing prose stays in `.md` files; binary carries them. | HIGH |

### Built-In (No Library Needed)

| Capability | Go built-in | Why no extra package | Confidence |
|------------|-------------|------------------------|------------|
| GitLab REST v4 client | `net/http` | 4 endpoints, `Link`-header pagination, `PRIVATE-TOKEN` header, `Retry-After` on 429. ~150 LOC including the normalizer. No `go-gitlab` SDK. | HIGH |
| LLM providers (OpenAI, Anthropic) | `net/http` | Both APIs are one POST + bearer token + JSON body. No SDK needed; we own the request shape and the response unmarshalling. Easy to add a third provider later. | HIGH |
| Config validation | Hand-rolled | A small validator on the loaded struct (required keys, URL parse, model name non-empty) is ~20 lines. No `validator/v10` dep for two checks. | HIGH |
| Argv parsing | `flag` | Subcommands are `flag.NewFlagSet` instances. ~30 LOC total for `summarizer run --project X --since 7d --dry-run --force --max-events 200 --audience customer`. | HIGH |
| Env var reads | `os.Getenv`, `os.LookupEnv` | `GITLAB_TOKEN`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`. No `viper` / `envconfig` needed. | HIGH |
| Tests | `testing` (stdlib) | Table-driven tests; `t.Run` for subtests; `httptest.NewServer` for fakes of GitLab + LLM. No `testify` needed (we use stdlib + maybe `testify/assert` for one-liners; defer the decision to plan-phase). | HIGH |
| Markdown output | `text/template` or plain `fmt.Sprintf` | Reports are generated content. No parser needed. | HIGH |
| Filesystem writes | `os.WriteFile` (tmp + rename) | `os.WriteFile(path.tmp, body, 0644)`; `os.Rename(tmp, path)` for atomicity. | HIGH |
| Single-binary build | `go build -o summarizer ./cmd/summarizer` | Static binary; no cgo; runs on any macOS / Linux with no install. `CGO_ENABLED=0` strips libc dependency. | HIGH |

### What NOT to Use

| Library / Pattern | Why Not |
|-------------------|---------|
| `spf13/cobra` | `flag` is enough for one subcommand + a handful of flags. Cobra adds a tree, a generator, and an opinionated help format. Defer until we have ≥3 subcommands. |
| `spf13/viper` | YAML + `os.Getenv` + hand-rolled validation is ~50 LOC. Viper adds ~10 transitive deps for config we don't have. |
| `go-gitlab` / `xanzy/go-gitlab` | 200+ transitive deps for 4 HTTP endpoints we can write in an afternoon. Pulls in a parser we don't need. |
| `sashabaranov/go-openai`, `anthropics/anthropic-sdk-go`, `tmc/langchaingo` | Same story: 4-30 deps for a single POST + JSON. We own the contract; pinning to a vendor's SDK means accepting their breaking-change cadence. |
| `go-playground/validator` | Two required-field checks is not worth a dep. |
| `testify/*` (suite, mock, require) | Stdlib `testing` + `httptest` covers MVP. Add `testify/assert` only if a specific test becomes painful in stdlib. |
| `zerolog` / `logrus` / `slog` (third-party) | `log/slog` (stdlib since Go 1.21) is enough. Writes structured JSON to stderr for the run summary. |
| `urfave/cli` | Same reason as Cobra. |
| Module-level globals / `init()` for config | Testability. Constructor injection only; `main.go` is the one place that wires concrete adapters to the pipeline's ports. |

### Project Layout

```
summarizer/
├── cmd/
│   └── summarizer/
│       └── main.go             # entry point; the ONLY place that imports every package
├── internal/
│   ├── config/
│   │   └── config.go           # YAML load, defaults, validation, env-var resolution
│   ├── gitlab/
│   │   ├── client.go           # net/http GitLab REST v4 client
│   │   └── types.go            # GitLab response structs (json tags only; no methods)
│   ├── events/
│   │   ├── event.go            # NormalizedEvent struct — the lingua franca
│   │   └── normalize.go        # GitLab JSON → []NormalizedEvent
│   ├── prompt/
│   │   ├── builder.go          # render prompt + audience + events → request
│   │   ├── sanitize.go         # OWASP LLM01 mitigations on event text
│   │   └── templates/
│   │       ├── customer.md     # //go:embed — customer voice
│   │       └── team.md         # //go:embed — team voice
│   ├── llm/
│   │   ├── provider.go         # Provider interface (the port)
│   │   ├── openai.go           # OpenAI adapter (net/http)
│   │   ├── anthropic.go        # Anthropic adapter (net/http)
│   │   └── usage.go            # parse Usage block; cap enforcement
│   ├── writer/
│   │   ├── writer.go           # Writer interface + atomic write (tmp + rename)
│   │   └── format.go           # filename conventions + run-summary struct
│   └── run/
│       └── pipeline.go         # Pipeline orchestrator; the ONLY cross-package coupling
├── go.mod
├── go.sum
├── examples/
│   └── summarizer.yaml
├── README.md
└── .planning/
```

### Development Tools

| Tool | Purpose | Notes |
|------|---------|-------|
| `go test ./...` | Unit + integration tests | Stdlib. `httptest.NewServer` for GitLab + LLM fakes. |
| `go vet ./...` | Static analysis | Stdlib. |
| `staticcheck ./...` | Stronger lints | Optional; defer to plan-phase. |
| `gofmt -s -w .` | Formatting | Stdlib. |
| `go build -o bin/summarizer ./cmd/summarizer` | Local build | Output goes in `./bin/`. |
| `CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o bin/summarizer ./cmd/summarizer` | Release build | Static binary, stripped, reproducible path. |
| `go mod tidy` | Module maintenance | Stdlib. |

### Module Setup

```bash
# In repo root:
go mod init github.com/inful/summarizer
go get gopkg.in/yaml.v3
```

`go.mod` declares Go 1.23 (or whatever the operator's `go version` reports). The only third-party direct dep is `yaml.v3`; everything else is stdlib.

## What Changed From The Initial Draft

The initial research output recommended Bun + TypeScript + Vercel AI SDK v7. After review, the operator locked the stack to Go. Differences from the draft:

- **Runtime / language** — Bun 1.4 → Go 1.23+. Removes the JS toolchain from the host.
- **HTTP client** — `fetch` (Web API in Bun) → `net/http` (stdlib). Same fetch semantics; stdlib gives a tiny perf edge and zero deps.
- **LLM SDK** — Vercel AI SDK v7 + `@ai-sdk/openai` + `@ai-sdk/anthropic` → direct `net/http` calls. Both OpenAI and Anthropic APIs are simple POST + bearer + JSON. Saves ~30 transitive deps and removes a vendor's breaking-change clock.
- **YAML** — `yaml@2.9.1` (eemeli) → `gopkg.in/yaml.v3` (industry standard; same role).
- **Schema validation** — `zod` → hand-rolled on the loaded struct (two checks; no dep).
- **CLI framework** — `Bun.argv` + stdlib → `flag` (stdlib).
- **Tests** — `bun test` → `go test` (both stdlib; same shape).
- **Single-binary build** — `bun build --compile --target=bun-darwin-arm64` → `CGO_ENABLED=0 go build -ldflags="-s -w"`.

## Installation

```bash
# Operator already has Go installed (per existing toolchain). Verify:
go version   # expect 1.23 or newer

# In the repo root:
go mod init github.com/inful/summarizer
go get gopkg.in/yaml.v3
```

## Open Questions

- **Go version floor.** Operator's `go version` will tell us. Use whatever is installed; pin in `go.mod` to that minor.
- **`go-playground/validator` vs hand-rolled.** Hand-rolled is fine for two required-field checks. Revisit if config grows.
- **`testify/assert` opt-in.** Stdlib `t.Errorf` is enough. Add only if a specific test hurts.
- **`staticcheck`.** Optional. Defer to plan-phase.
- **Module path.** `github.com/inful/summarizer` assumed; confirm at first commit.

## Sources

### Primary (HIGH confidence)
- Go stdlib docs: `net/http`, `flag`, `os`, `embed`, `text/template`, `log/slog`, `testing`, `httptest` — verified against the Go 1.23 release notes and current pkg.go.dev pages.
- `gopkg.in/yaml.v3` v3.0.1 — verified current via GitHub release tags.

### External (HIGH confidence)
- GitLab REST API v4 docs — `Link`-header pagination, `Retry-After`, `PRIVATE-TOKEN` header.
- OpenAI Chat Completions API — auth, request/response shape, `usage` block.
- Anthropic Messages API — auth, request/response shape, `usage` block.

### Tertiary (LOW confidence — needs validation during implementation)
- Operator's exact Go version (will be discovered at `go.mod` initialization).
- Whether the operator prefers `flag` (stdlib) vs `cobra` for any future CLI growth. Default = stdlib.