---
name: add-handler
description: "Add an HTTP handler to THIS repo's exact layout: handler func + writeJSON/writeError in internal/http/handlers.go, route line in registerRoutes, table case in TestRoutes (handlers_test.go), then build/vet/test. Use when the user says 'add a handler', 'add a route', 'new endpoint', 'expose GET/POST /...'. For a general Go project with services/DTOs use /go-api-endpoint instead."
argument-hint: "<METHOD> </api/v1/path/{param}> [handlerName]"
arguments: [method, path, handler]
shell: powershell
allowed-tools: PowerShell(go build *) PowerShell(go vet *) PowerShell(go test *) PowerShell(go version) PowerShell(golangci-lint run *) PowerShell(Get-Content *) PowerShell(Get-ChildItem *) PowerShell(Select-String *) PowerShell(Measure-Object *) PowerShell(Select-Object *) PowerShell(Write-Output *) Bash(go build *) Bash(go vet *) Bash(go test *) Bash(golangci-lint run *)
---

# add-handler

Method: `$method`  Path: `$path`  Handler: `$handler` (empty -> derive: `POST /api/v1/things` -> `handleCreateThing`).

## Current wiring
!`Get-Content go.mod -TotalCount 1; Select-String -Path internal\http\handlers.go -Pattern 'mux\.HandleFunc\('; Write-Output 'test table rows:'; Select-String -Path internal\http\handlers_test.go -Pattern '^\s*\{"' | Measure-Object | Select-Object -ExpandProperty Count; Get-ChildItem internal\http -Filter '*.go' -Name; go version`

## Files (do not invent others)
| What | Where |
|---|---|
| handler func `handleX` (+ request/response structs) | `internal/http/handlers.go` |
| route line | `registerRoutes` in `internal/http/handlers.go` (the single route table) |
| test case | `TestRoutes` table in `internal/http/handlers_test.go` |
| endpoint list | `README.md` "Endpoints" section |

## Procedure
1. Read `internal/http/handlers.go` and `handlers_test.go` first; match the existing `handlePing` /
   `handleReadyz` style (unexported, `http.HandlerFunc`, closures for injected deps).
2. **Handler** in `handlers.go`, next to its siblings:
   - Request body: `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)`, `json.NewDecoder(r.Body)` with
     `DisallowUnknownFields()`; decode error -> `writeError(w, http.StatusBadRequest, "invalid json")`.
   - Path params: `r.PathValue("id")`. Validation failures -> 400 with the field name in the message.
   - Response: a named struct with json tags (see `pingResponse`), written via `writeJSON`.
     201 + `Location` for creates, 200 for reads, 204 for deletes.
   - Dependencies: declare a small interface in the handlers package, take it as a parameter
     (`handleGetThing(store ThingStore) http.HandlerFunc`), add it as a parameter of
     `registerRoutes` and thread it through `NewRouter` -> `NewRouterWithReadiness` ->
     `NewRouterWithReadinessAndTracing`. Never touch a DB directly in the handler.
   - Log only through `slog` with `RequestIDFrom(r.Context())`; never return `err.Error()` to the client.
3. **Route**: add `mux.HandleFunc("$method $path", handleX)` inside `registerRoutes` in `handlers.go`,
   next to the existing lines listed above. That is the only route table (every `NewRouter*` constructor
   builds on it, so tests and prod see the same routes); do not add `mux.HandleFunc` anywhere else.
   `{param}` segments and trailing slashes must be unambiguous with siblings.
4. **Test**: add rows to the `TestRoutes` table (happy path; wrong method -> 405; bad body/param -> 400;
   not found -> 404 when applicable). If the handler needs a dependency, add a hand-written fake in
   `handlers_test.go` and extend `testRouter` to pass it. Keep `t.Parallel()` on every subtest.
5. **README**: append the endpoint under "Endpoints".
6. **Verify** (report exactly what ran):
   ```powershell
   go build ./...; go vet ./...; go test -count=1 ./internal/http/...
   golangci-lint run ./internal/http/...
   ```
   Smoke if the server is up: `Invoke-RestMethod -Method $method http://localhost:8080$path`.

## Done when
Route registered, test rows pass, `go vet` + lint clean, README updated. Report files touched,
the exact pattern string, and the interface signature if one was added.
