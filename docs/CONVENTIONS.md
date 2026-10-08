# Conventions specific to this repo

Moved verbatim from CLAUDE.md. See [../CLAUDE.md](../CLAUDE.md) for the short invariant list.

- **Frozen types** (changed only by the integrator, together with `docs/PLAN.md`): `store.Meta`, `store.Writer`,
  `cache.Store`, `cache.Fetcher`, `cache.Recorder`, `rfc9111.Resp`, `rfc9111.Decision`, `config.Config`. A lane
  that needs a change files it in its report instead of editing in place. Every other exported signature in the
  step-1 stubs is the contract the lanes compile against: replace bodies, keep signatures, add freely.
- Ports are consumer-defined (`cache.Store`, `cache.Fetcher`, `cache.Recorder`, `server.Purger`, `tiered.Tier`);
  `cmd/edgecache/ports_test.go` is where satisfaction is asserted, not in the implementing package.
- The proxy handler is mounted **without** `Timeout`/`http.TimeoutHandler` (aigw's streaming lesson); its budget
  is the origin context. Only the admin listener goes through the full template stack.
- A miss streams: one loop writes to the store writer and the client; a client write error does not abort the
  fetch (up to `ORPHAN_MAX_BYTES`). The collapse leader runs on a root-derived context, never the caller's.
- Metric labels are fixed enums only (never a URL, never a client header). Responses carry
  `X-Cache: HIT|MISS|STALE|REVALIDATED|BYPASS`, `Age`, `X-Cache-Reason` on bypass, `X-Cache-Key` in debug builds.
- Every deviation from the RFC is a named config key defaulting to spec-compliant (`CACHE_AUTHORIZATION=false`,
  `DEFAULT_SWR=0s`). `Warning` is never emitted.
- Errors to clients on the admin listener go through `writeError` (stable strings); internals are logged with the
  request id, never returned in the body.
- `.golangci.yml` is v2 with gofumpt+goimports formatters; hooks format on save, don't run them by hand. Every
  exported identifier is documented (revive `exported`). Don't fix an "unused" stub with `//nolint`.

