---
name: db
description: "Goose + sqlc helper bound to this repo's paths (db/migrations, db/queries, sqlc.yaml -> internal/db/sqlc) and its compose postgres (postgres://app:app@localhost:5432/app). Actions: bootstrap, create <name>, up, down, status, redo, gen. Use when the user says 'add a migration', 'new table', 'sqlc generate', 'migrate', or edits db/*.sql here. The general cross-project version is /go-migrate."
argument-hint: "<bootstrap | create <name> | up | down | status | redo | gen>"
arguments: [action, name]
shell: powershell
allowed-tools: PowerShell(goose *) PowerShell(sqlc *) PowerShell(go build *) PowerShell(go vet *) PowerShell(go test *) PowerShell(go version) PowerShell(docker compose *) PowerShell(Get-Command *) PowerShell(Get-ChildItem *) PowerShell(Select-Object *) PowerShell(Write-Output *) Bash(goose *) Bash(sqlc *) Bash(go build *) Bash(go test *) Bash(docker compose *)
---

# db

Action: `$action`  Name: `$name`

## State
Tools under `tools found:` exist; goose / sqlc not listed are MISSING (`go install github.com/pressly/goose/v3/cmd/goose@latest`, `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`). No `sqlc.yaml` listed -> run bootstrap. No migrations listed -> `db/migrations` is empty/absent. Check `$env:DATABASE_URL` yourself (unset -> compose default). Blank compose status = stack down / daemon down.
!`Write-Output 'tools found:'; Get-Command goose, sqlc -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name; Write-Output 'sqlc config:'; Get-ChildItem -Filter 'sqlc.*' -Name -ErrorAction SilentlyContinue; Write-Output 'latest migrations:'; Get-ChildItem db\migrations -Name -ErrorAction SilentlyContinue | Select-Object -Last 5; Write-Output 'compose:'; docker compose ps --format '{{.Service}} {{.Status}}' 2>$null; go version`

## Repo-bound facts
- Migrations: `db/migrations/<timestamp>_<snake>.sql` (goose, `-- +goose Up` / `-- +goose Down`).
- Queries: `db/queries/<domain>.sql` (`-- name: GetThing :one`). Generated: `internal/db/sqlc/`
  (package `sqlc`, pgx/v5, committed, excluded from lint, never hand-edited).
- Schema source for sqlc is `db/migrations` (single source of truth).
- DSN: `$env:DATABASE_URL` if set, else the compose default
  `postgres://app:app@localhost:5432/app?sslmode=disable` (matches `compose.yaml`).
  Set once per command batch: `$env:GOOSE_DRIVER='postgres'; $env:GOOSE_DBSTRING=<dsn>; $env:GOOSE_MIGRATION_DIR='db/migrations'`.
- Postgres not reachable -> `task up` (or `/docker-up`), then retry. Do not loop.

## Actions
**bootstrap** (first time): create `db/migrations/`, `db/queries/`; write `sqlc.yaml`:
```yaml
version: "2"
sql:
  - engine: postgresql
    schema: db/migrations
    queries: db/queries
    gen:
      go:
        package: sqlc
        out: internal/db/sqlc
        sql_package: pgx/v5
        emit_json_tags: true
        emit_empty_slices: true
```
Then `go get github.com/jackc/pgx/v5`, add to `.golangci.yml` `exclusions.paths` -> `internal/db/sqlc`,
and add Taskfile targets `migrate:up|down|status|create` and `sqlc` mirroring the commands below.

**create `<name>`**: `goose -dir db/migrations create $name sql`, then fill Up and Down.
Down is real (never a no-op) and every `DROP ...` uses `IF EXISTS`. Indexes ship in the same
migration as their FK. `NOT NULL` on an existing table needs a `DEFAULT` or a backfill step.

**status / up / down / redo**:
```powershell
goose -dir db/migrations postgres "<dsn>" status
goose -dir db/migrations postgres "<dsn>" up
goose -dir db/migrations postgres "<dsn>" down     # one step
goose -dir db/migrations postgres "<dsn>" redo     # proves Down works
```
`relation already exists` -> stop, show `status`, do not force.

**gen** (also after create+up): `sqlc vet; sqlc generate; go build ./...; go vet ./...`.
Fix sqlc errors in the SQL, not in generated Go.

## Done when
`goose status` shows nothing pending, `redo` passes, `sqlc generate` + `go build ./...` + `go vet ./...`
are clean. Report migration file paths, current version, and generated `Queries` methods added.
