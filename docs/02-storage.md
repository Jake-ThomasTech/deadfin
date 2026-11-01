# 02 — Storage

Decisions: fresh Go-native schema (D3), two-backend design shipping SQLite (D5).
Upstream reference: EF Core wide-table + JSON-blob dual-write
(`BaseItemMapper.cs:217-220, 436-470`), `ItemValues` intern table, `AncestorIds`
closure table, 30 DbSets. Upstream stores genres/tags/artists in **three** places
and hierarchy in **four**; a commented-out normalized schema in
`JellyfinDbContext.cs:176-260` shows upstream knows the current design is bad.
Deadfin normalizes.

## Schema principles

1. **Every fact stored once.** `items` = typed columns for every filterable/
   sortable field (~60) + one `extra` JSON column for non-queried per-kind payload.
   No serialized-object column, no CSV mirror columns.
2. **Interned values:** `item_values(id, kind, value, clean_value)` with
   `kind ∈ {genre, tag, artist, album_artist, studio, production_location, …}` +
   `item_value_map(item_id, value_id, role)`.
3. **Hierarchy:** `parent_id` is truth. `ancestors(item_id, ancestor_id)` closure
   table, `top_parent_id`, `series_id`, `season_id` are **derived**, maintained
   transactionally by library writes, with a repair task. Each write repairs
   only the written items and their descendants (`repairDerivedForTx`); the
   global repair runs only for a derived-state version change or an explicit
   maintenance call. On PostgreSQL, writers take hierarchical advisory keys
   (exclusive on written items, shared on their ancestors) instead of one
   global lock; `item_derived_lock.go` records the reasoning. (Recursive CTEs per
   /Items query rejected: measurably worse for the hot path on both engines.
   Upstream's sin was undeclared ownership, not redundancy.)
4. **`user_data(user_id, custom_key, item_id, played, position_ticks, play_count,
   is_favorite, rating, likes, audio_stream_index, subtitle_stream_index,
   last_played, …)`** keyed `(user_id, custom_key)` — mirrors upstream's
   ItemId+CustomDataKey model so shared watch-state via `UserDataKeys` works and
   the importer has a stable join key.
5. **`item_shares(item_id, user_id, can_edit)`** — normalizes Playlist shares.
   Playlist visibility uses its distinct `open_access` and
   `playlist_owner_user_id` facts plus `EXISTS(share)` in SQL; generic item
   `owner_id` is not a playlist-owner alias. BoxSet visibility instead uses
   `boxset_library_folders` plus the later parental/child checks. This fixes the
   upstream in-memory landmine (divergence D-03; observable results identical).
6. Satellite tables: `media_streams` (per-stream probe results, ~40 cols incl.
   HDR/DV fields), `media_attachments`, `chapters`, `people` + `people_map(role,
   sort_order, list_order)`, `trickplay_info`, `keyframes`, `media_segments`, `users`,
   `devices` (**token = device row**, upstream model kept), `api_keys`,
   `display_preferences`, `activity_log`, `libraries` (registry row per virtual
   folder; the filesystem `options.xml`/`.collection`/`.mblink` topology remains
   the source of truth for *contents* — compatible with upstream).
7. Search columns: `clean_name` on items, `clean_value` on item_values —
   precomputed by `internal/item.CleanName` from the pinned `Diacritics` map,
   .NET invariant lowercase, UTF-16 letter/number/whitespace filtering,
   whitespace collapse, and trim. Both engines store identical bytes matching
   upstream `StringExtensions.GetCleanValue()`.

### DDL sketch (SQLite dialect; PG differs only in types)

```sql
CREATE TABLE items (
  id            BLOB PRIMARY KEY,            -- 16-byte UUID (PG: uuid)
  kind          TEXT NOT NULL,               -- BaseItemKind string
  parent_id     BLOB,
  top_parent_id BLOB,                        -- derived
  series_id     BLOB, season_id BLOB,        -- derived
  path          TEXT,                        -- %AppDataPath%/%MetadataPath% tokens kept
  name          TEXT, sort_name TEXT, clean_name TEXT,
  original_title TEXT, forced_sort_name TEXT,
  overview      TEXT, tagline TEXT,
  premiere_date INTEGER, end_date INTEGER,   -- unix micros UTC (PG: timestamptz)
  date_created  INTEGER NOT NULL, date_modified INTEGER NOT NULL,
  date_last_refreshed INTEGER, date_last_saved INTEGER,
  date_last_media_added INTEGER,
  production_year INTEGER, index_number INTEGER, parent_index_number INTEGER,
  run_time_ticks INTEGER, size INTEGER, width INTEGER, height INTEGER,
  container TEXT, total_bitrate INTEGER,
  community_rating REAL, critic_rating REAL,
  official_rating TEXT, custom_rating TEXT,
  inherited_parental_value INTEGER, inherited_parental_subvalue INTEGER,
  unrated_type TEXT,
  is_folder INTEGER NOT NULL DEFAULT 0,
  is_virtual INTEGER NOT NULL DEFAULT 0,
  is_in_mixed_folder INTEGER NOT NULL DEFAULT 0,
  is_locked INTEGER NOT NULL DEFAULT 0,
  media_type TEXT, video_type TEXT, extra_type TEXT,
  presentation_unique_key TEXT, series_presentation_unique_key TEXT,
  primary_version_id BLOB, owner_id BLOB, playlist_owner_user_id BLOB NOT NULL, channel_id BLOB,
  album TEXT, album_artists TEXT_IS_WRONG,   -- NO: artists/albums via item_values
  external_id TEXT, external_series_id TEXT,
  preferred_metadata_language TEXT, preferred_metadata_country TEXT,
  lufs REAL, normalization_gain REAL,
  open_access INTEGER NOT NULL DEFAULT 0,
  extra JSON NOT NULL DEFAULT '{}'           -- remote trailers and non-query
                                             -- per-kind Data facts
);
-- NOTE for implementer: the album_artists line above is deliberately marked wrong
-- as a reminder — artists/album-artists live ONLY in item_values (kind=artist/
-- album_artist). `album` (display string) stays a column: it is sorted/filtered on.

CREATE INDEX ix_items_parent   ON items(parent_id);
CREATE INDEX ix_items_kind_top ON items(kind, top_parent_id);
CREATE INDEX ix_items_clean    ON items(clean_name);
CREATE INDEX ix_items_prezkey  ON items(presentation_unique_key);
CREATE INDEX ix_items_path     ON items(path);
-- + partial/sort-supporting indexes added per query-corpus profiling in P2.

CREATE TABLE item_images (item_id BLOB, type TEXT, idx INTEGER, path TEXT,
  date_modified INTEGER, width INTEGER, height INTEGER, blurhash TEXT,
  PRIMARY KEY(item_id, type, idx));
CREATE TABLE provider_ids (item_id BLOB, provider TEXT, value TEXT,
  PRIMARY KEY(item_id, provider));
CREATE TABLE locked_fields (item_id BLOB, field TEXT, PRIMARY KEY(item_id, field));
CREATE TABLE item_values (id INTEGER PRIMARY KEY, kind TEXT NOT NULL,
  value TEXT NOT NULL, clean_value TEXT NOT NULL, UNIQUE(kind, value));
CREATE TABLE item_value_map (item_id BLOB, value_id INTEGER,
  sort_order INTEGER NOT NULL,
  PRIMARY KEY(item_id, value_id));
CREATE INDEX ix_item_value_map_item_order
  ON item_value_map(item_id, sort_order, value_id);
CREATE TABLE linked_children (parent_item_id BLOB NOT NULL,
  child_item_id BLOB NOT NULL, type TEXT NOT NULL, sort_order INTEGER,
  PRIMARY KEY(parent_item_id, child_item_id));
CREATE INDEX ix_linked_children_child ON linked_children(child_item_id);
CREATE INDEX ix_linked_children_parent_order
  ON linked_children(parent_item_id, sort_order, child_item_id);
CREATE TABLE ancestors (item_id BLOB, ancestor_id BLOB,
  PRIMARY KEY(ancestor_id, item_id));
CREATE TABLE user_data (user_id BLOB, custom_key TEXT, item_id BLOB,
  played INTEGER NOT NULL DEFAULT 0, position_ticks INTEGER NOT NULL DEFAULT 0,
  play_count INTEGER NOT NULL DEFAULT 0, is_favorite INTEGER NOT NULL DEFAULT 0,
  rating REAL, likes INTEGER, last_played INTEGER,
  audio_stream_index INTEGER, subtitle_stream_index INTEGER,
  PRIMARY KEY(user_id, custom_key));
CREATE TABLE item_shares (item_id BLOB, user_id BLOB, can_edit INTEGER,
  PRIMARY KEY(item_id, user_id));
-- media_streams, media_attachments, chapters, people, people_map, users, devices,
-- api_keys, display_preferences, activity_log, trickplay_info, keyframes,
-- media_segments, libraries: columns per upstream entities (recon report),
-- normalized the same way. Full DDL is authored in WP-P1-01/WP-P2-02 migrations.
```

## Query layer

**DECISION: `database/sql` + sqlc for fixed-shape statements + a hand-built
translator for `ItemQuery`. No ORM.**
RATIONALE: ~90% of statements (CRUD, upserts, satellite reads) are static — sqlc
gives them types per dialect for free. The /Items query is one large dynamic
translation; upstream proves it's a single coherent function
(`BaseItemRepository.TranslateQuery.cs`, 1,258 lines). Port it as
`store/itemquery_sql.go: Translate(q *ItemQuery, d Dialect) (sql string, args
[]any)` using `strings.Builder`, decomposed into ~30 `applyXxx` funcs per filter
family, each unit-tested.
REJECTED: GORM (dialect behavior drift, reflection cost on 155-prop scans, dynamic
queries end up hand-written anyway); squirrel (the hard part is translation logic,
not string assembly).

- `store.ItemQuery`: ports only the **~140 honored** properties of
  `InternalItemsQuery` (the full 144 enumerated in the recon; the dead ones listed
  as intentionally dropped in a comment). Slices = IN lists; pointers = tri-state.
- 30 sort fields (`ItemSortBy`): the user-data-dependent ones (IsPlayed,
  DatePlayed, PlayCount, IsFavoriteOrLiked) via correlated subqueries on
  `user_data`, shared across dialects; `Random` via `Dialect.Random()`;
  `PremiereDate` falls back to ProductionYear like upstream (`OrderMapper.cs`).
  **Every ORDER BY ends with `, id`** (deterministic pagination — upstream doesn't
  guarantee this; divergence D-04, invisible except to flaky-pagination bugs).
- Grouping: `GroupByPresentationUniqueKey` / `SeriesPresentationUniqueKey` and
  box-set collapsing in SQL (GROUP BY + MIN(id) preferring
  `primary_version_id IS NULL`, mirroring `ApplyGroupingFilter`,
  `QueryBuilding.cs:63-104`); enable/disable conditions replicated exactly
  (disabled when IsResumable, when PresentationUniqueKey set, when no user; only
  for Episode/Video/Movie/MusicVideo/Series/Season or untyped queries).
- Search: `LIKE` over `clean_name`/`clean_value` for parity (upstream has **no
  FTS**); relevance = CASE ranking (exact=0 < prefix-space=1 < prefix=2 <
  contains=3, `OrderMapper.MapSearchRelevanceOrder`). `Dialect.SearchClause` seam
  reserved for future FTS5/tsvector — not built in phase 1.
- `Dialect` interface: placeholder style, boolean literals, datetime scan/value,
  `Random()`, LIKE-escape, `NULLS LAST` emission, GROUP BY strictness. **Write
  PG-strict SQL from day one** (explicit column lists) so PG later is a dialect,
  not a rewrite.

## SQLite semantics containment (the leak-proof list)

- Driver: **`modernc.org/sqlite` (pure Go).** Cross-compile + CI simplicity; cgo
  stays confined to `internal/images`. Revisit only if the P2 perf gate (10k-item
  /Items p95 vs stock on same hardware) fails; store seam makes a
  `mattn/go-sqlite3` swap a one-package change.
- Connection contract: WAL, `busy_timeout=5000`, `foreign_keys=ON`,
  `synchronous=NORMAL`; **one writer `*sql.DB` (MaxOpenConns=1) + reader pool**,
  enforced by the store constructor. Matches the deployment reality (SQLite on
  Longhorn block storage; three years uneventful) and sidesteps SQLITE_BUSY
  classes.
- Collation/case: **never** rely on SQLite NOCASE (ASCII-only) or PG collations —
  all insensitive matching via Go-precomputed `clean_*` columns. This is the
  single most important leak preventer: collation bugs produce *wrong results*,
  not errors.
- Datetimes: INTEGER unix-microseconds UTC in SQLite / `timestamptz` in PG,
  converted in Dialect helpers. No ISO-8601 TEXT dates (string comparison
  semantics + index size). Upstream stores EF TEXT dates — the importer converts.
- NULL ordering: explicit `NULLS LAST`/`FIRST` everywhere (SQLite ≥ 3.30 supports
  the syntax; engine defaults differ between SQLite and PG).
- Booleans INTEGER 0/1 (PG: boolean, via Dialect). UUIDs BLOB(16) (PG: uuid).
- LIKE: SQLite LIKE is ASCII-case-insensitive by default, PG LIKE is sensitive —
  irrelevant because matching happens on pre-lowered clean columns; the translator
  never emits LIKE against a non-clean column (CI grep).
- Concurrent writes: single-writer contract above; PG later gets ordinary
  connection pooling behind the same store interface (the contract is an
  implementation detail of store/sqlite, not of the interface).
- Planner statistics: the server store owns a statistics runner on the
  writer connection. It runs `PRAGMA analysis_limit=400; PRAGMA
  optimize=0x10002` at start, every 3 hours, after a scan that changed at least
  1,000 items, and on `Close`. It runs a full `ANALYZE` instead when
  `sqlite_stat1` has no `items` row or its stored `items` row count is off by
  more than 2×. SQLite loads statistics only at schema load and `ANALYZE` does
  not change the schema cookie, so a run that changes `sqlite_stat1` also
  makes a no-op schema change; the reader pool then reloads the statistics.
  PostgreSQL needs no runner: autovacuum analyzes each table after 10% + 50
  row changes.
- Page cache and memory map: `DEADFIN_SQLITE_CACHE_MB` sets `cache_size` and
  `DEADFIN_SQLITE_MMAP_MB` sets `mmap_size` on every writer and reader
  connection. Unset keeps the SQLite defaults (2 MiB cache, no map). The cache
  is per connection, so the reader pool can hold up to five times the value.

## Migrations

**DECISION: goose**, embedded via `embed.FS`, auto-migrate on boot (upstream
parity), per-dialect directories (`migrations/sqlite`, `migrations/postgres`),
Go-func migrations allowed for data transforms (clean_* recompute).
REJECTED: atlas (external tool, diff workflow fights hand-tuned per-dialect SQL);
golang-migrate (no ergonomic Go-func migrations).

On PostgreSQL, `migrate.Up` holds a session advisory lock (one key per
effective `search_path`) on a separate two-connection pool, so servers that
start together against one database apply each migration once.

## Backend selection

`DEADFIN_DATABASE_URL` (or `--database-url`) selects the backend for the
server and every maintenance command, through one opener in
`cmd/deadfin/database.go`. Empty selects SQLite at `<data>/deadfin.db`. A
`postgres://` or `postgresql://` URL selects PostgreSQL with that URL as the
pgx connection string. Any other scheme fails at start. Logs show the URL
with the password replaced by `xxxxx`. The SQLite statistics runner and the
`DEADFIN_SQLITE_*` cache settings apply only to SQLite.

## storetest — the backend conformance suite

`internal/store/storetest`: exported suite taking a `func(t) store.Stores`
factory; run in CI against sqlite (memory + file); later PG via testcontainers.
Contents:
1. Fixture library (~300 items: all kinds, a series tree, multi-version movie,
   boxset, playlists with shares, people/genres/studios, mixed folders) seeded
   through the public store API.
2. Table-driven ItemQuery corpus: several hundred `(query → expected ordered ID
   list)` cases covering every filter family, every sort field, grouping/collapse,
   ancestor scoping, share visibility, pagination windows.
3. Property checks: pagination stability under equal sort keys (the `, id`
   tiebreak), ancestor-closure integrity after move/delete, value-intern
   dedupe, user_data upsert semantics.

This suite and the containment list above show that PostgreSQL fits in one work
package. That work package contains:
`store/postgres` Dialect + migrations, run storetest green, then the chained
differential below.

## PG proof chain (designed now, runs later)

Tier-1 differential can only anchor **Deadfin-on-SQLite ↔ upstream** (no upstream
PG exists). PG correctness is a second chained proof:

```
upstream (SQLite)  ⇐diff⇒  Deadfin/SQLite  ⇐diff⇒  Deadfin/PG
        (T2 harness, continuous)      (storetest + difftool with DEADFIN_DB=pg)
```

The second link reuses the same difftool corpora with Deadfin-vs-Deadfin targets.
**Weakest link, stated:** behaviors *not covered by any corpus* — timing-dependent
paths (transcode lifecycle under load, WS cadence) and write-path races — get no
transitive guarantee; the single-writer SQLite contract vs PG's MVCC is the most
likely divergence site (e.g. read-your-write visibility during scans). Mitigation
when the PG phase arrives: a write-heavy scan-while-querying scenario added to
storetest, and a soak run of the full T2 corpus against PG under concurrent scan.

## Importer (`deadfin import-jellyfin`, `internal/jfimport`)

**DECISION: import identity + user state; re-scan media; match items rather than
import them.**
Imported: `users` (PBKDF2-compatible hash verify ported — passwords survive),
`devices` incl. AccessTokens (sessions survive cutover), `api_keys`, `user_data`,
`display_preferences`, shares (extracted from item JSON blobs → `item_shares`),
optionally `activity_log`.
User ids: each imported user gets a fresh random id. The importer never keeps
the Jellyfin user id, because clients such as Swiftfin key saved users by user
id alone and confuse two servers that share one. Every row the importer writes
(devices, user data, playlist owners and shares) uses the new id. A rerun
matches users by normalized name, so it never duplicates a user. Device access
tokens keep their value. `deadfin remap-user-ids` gives every user of an
already-imported server a new id and rewrites every user-id column
(`store.UserIDReferences`) in one transaction; it records the run in
`store_metadata`, so a second run does nothing. A running server keeps live
sessions under the old ids in memory: stop it first, or restart it at once
after the run while nothing plays. Clients
keep their tokens but see a new user id, so Swiftfin needs one new sign-in.
Matching: snapshot `old_item_id → (path, type FQN, provider_ids,
presentation_key, custom_data_keys)` from upstream `BaseItems`; after Deadfin's
own scan of the same media roots, join user_data by custom-data-key (provider-
derived keys are scan-stable), else by path, else by ProviderIDs (catches moved
files); emit an unmatched report with counts per reason.
Locked metadata: report items with non-empty LockedFields; `--import-locked-
overrides` copies those fields + lock flags onto matched items.
Playlists/BoxSets: contents recovered by normal scan from the filesystem; shares
from import.
RATIONALE: full item import drags 70 columns + JSON-blob fidelity + FQN types +
upstream's data bugs into the fresh schema. Rescan guarantees Deadfin invariants
from day one; what users fear losing is watch state, and that is imported.
TRADE-OFF: hours of rescan on 20 TiB + remote-metadata refetch. Mitigations: NFO-
first (library already NFO-rich after years of upstream), provider caching (P8),
and the rehearsal in P10 measures the real cost before cutover.
FALLBACK: full item import, only if the P10 rehearsal shows unacceptable match
drift (< 99% user_data matched).
Acceptance gate: **≥ 99% of user_data rows matched** on the real 282 MB db, and
watch-state spot checks pass from real clients (P10 exit).

Source-side notes for the importer author: upstream `BaseItems.Type` is a .NET
FQN (mapping table in `ItemTypeLookup.cs` is non-bijective — resolve LiveTv
collisions to the LiveTv kinds, skip kinds Deadfin stubs); dates are EF TEXT
ISO-8601 → convert to unix micros; paths may contain `%AppDataPath%` /
`%MetadataPath%` tokens — keep tokens, they're portable; `UserData.CustomDataKey`
join semantics per `Entities/UserData.cs`.
