# easyvcs

A Change-Native version control engine, extracted from EasyLab as a standalone,
dependency-light Go module.

## Concept

`Change` (a logical modification) is the first-class entity with a stable id
(`change_id` / `revision_id`) that **never** changes, no matter how often it is
rebased, squashed, or amended. The content version (`Snapshot`) is
content-addressed and mutable via rewrite. Refs are bookmarks (branches, mutable)
and tags (immutable).

- `Snapshots` are content-addressed graph nodes (`sha` derived from parents +
  tree + description + author). Rebasing changes the `sha` but never the
  `change_id`.
- `Tree` / `blob` / `conflict` (first-class conflict objects) make up the object
  model, with 3-way tree merges.
- Pluggable storage via `store.Store`: `FileStore` (local `.easyvcs/` directory)
  and `SqlStore` (SQLite / Postgres) share one interface; content can live in
  the DB too (no format-must-match-Git constraint).

## Packages

- `object` — content-addressed primitives (`blob`, `tree`, `conflict`) and ids
- `store` — the persistence boundary (`Store` interface, `FileStore`,
  `SqlStore`, `CentralStore`, `Repo`)
- `revision` — the semantic layer (commit / amend / rebase / squash / merge /
  resolve / drop / revert / diff), stable-id rules live here
- `merge` — 3-way tree merge producing first-class conflicts
- `transfer` — serialization / bundling (codecs, deltas)
- `mirror` — push/pull mirror scheduling
- `encoding` — snapshot/change headers encoding
- `ignore` — gitignore-style matching
- `webapi` — optional HTTP/JSON API server (no connect/proto dependency)

## CLI

```
easyvcs init [DIR]
easyvcs commit [--new] [DIR]
easyvcs amend [DIR]
easyvcs log / show / diff / checkout
easyvcs rebase <change> --onto <sha>
easyvcs branch <name> <revision>     (bookmark)
easyvcs tag <name> <revision>        (immutable)
easyvcs refs / rev <expr>
```

## Build

```
CGO_ENABLED=0 go build -o easyvcs ./cmd/easyvcs
```

## Module

`github.com/easylab-platform/easyvcs`. It is consumed as a library by
`github.com/easylab-platform/easylab` (the server, agent bridge, and ops
platform).
