---
name: block-unguarded-drop-table
enabled: true
event: file
action: block
conditions:
  - field: file_path
    operator: regex_match
    pattern: db[\\/]migrations[\\/][^\\/]+\.sql$
  - field: content
    operator: regex_match
    pattern: \bDROP\s+TABLE\s+(?!IF\s+EXISTS\b)
---

**`DROP TABLE` without `IF EXISTS` in a goose migration. Blocked.**

Down migrations must be re-runnable (`goose redo` runs Down then Up). A bare
`DROP TABLE x` fails when the table is already gone and leaves the version
table inconsistent.

Write `DROP TABLE IF EXISTS x;` instead. Same rule of thumb for
`DROP INDEX IF EXISTS` / `DROP TYPE IF EXISTS`.
