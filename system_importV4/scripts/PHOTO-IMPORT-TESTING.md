V4 legacy image import
======================

The destination is Dr.Ease V4. `system_CLIV4` and `system_importV4` happen to live
in the legacy repository. No legacy production application code is changed.

Deploy V4 migrations 0420 and 0798 before using manifest version 2. The matching
V4 media query keeps imported forms/drawings out of E5. No migration rebinds or
reclassifies existing patient data.

Mapping
-------

- Use `page.vd_first` as the examination day (YYYY-MM-DD or DD/MM/YYYY, Gregorian).
  Keep revision `created_at` separately. Invalid/missing examination days use the
  Bangkok calendar day of the latest scan, as requested on 2 Nov 2026. If both
  dates are unusable, skip with a review warning; never invent 2026-01-01. The
  metadata `_visit_date_source` records `vd_first` or `scan`. A date range uses its first day, while
  retaining both original dates in metadata.
- Reuse the latest open V4 encounter for clinic/HN/day, ordered by `created_at`
  descending and then id descending, as requested on 2 Nov 2026. Deleted and
  signed encounters are excluded. Create one only when there is no eligible
  encounter. Deleted VN allocations remain spent.
- `before`/`after`, docgroup 29 and the known BA2/BA6/BA10/BAV composites are
  Before–After. Composite pages keep kind `note`: their side is not invented.
- OPD doctypes are legacy form images. Explicit drawing doctypes are drawings.
  They become separate E1 photo pages, retaining the original raster image.
  Existing V4 ink, page selection and pins are preserved when adding pages.
  Exact repeats do not restore chart pages a clinician removed. Visit rows are
  locked using V4's own save order before inspection; signed source records
  reject moves or changes. New pages use a separate open encounter when needed. Real Before–After changes advance E5's optimistic version.
- Unknown doctypes remain `unknown` in the patient gallery, with a review warning.
  Configure site-specific codes using CLI `-page-type-map '{"CODE":"drawing"}'`
  (allowed values: before_after/form/drawing), or `PhotoRunOptions.pageTypeMap`.
  Merely having annotation data does not prove the originating function.
- Full original page metadata, including exact doctype, VN, page/revision ids and
  `anno`, is stored in V4 `photos.legacy_metadata`. Legacy `anno` is preserved as
  source data; this change does not translate its format into editable V4 strokes
  or rebuild scanned forms as native fields. Such data is explicitly reported.
- Pick the newest timestamped revision per source page, retaining all distinct
  pages. Stable page identity replaces the timestamp/size deduplication heuristic.
  Newer revisions update the same photo id only while the V4 record is untouched.
  Changes to V4 ink/pins or image records block automatic replacement. Older dumps
  cannot roll back a newer imported revision. Exact re-runs add nothing.
- Existing imports are matched by clinic, HN and exact revision-derived paths,
  including historical revision filenames in `_legacy_file_paths`. Adopt matched
  rows and refile them into the selected day/encounter while keeping the photo id.
  Store the prior visit id in metadata. Remove moved page markers from the old
  chart and add them to the target chart; keep the original encounters. Source
  records with V4 edits, pins, ink, signatures or deletion markers refuse the
  transaction. Ambiguous file matches also refuse. Unmatched older rows remain
  unchanged; a different legacy naming salt must be supplied explicitly. A stale
  dump cannot regress a newer revision or move it back to an earlier encounter.
- File publication requires `sudo -n python3` on the uploads host. Images and
  thumbnails are SHA-256 checked in a private staging directory, then linked into
  place atomically without replacement. Existing files must have identical bytes;
  their content, timestamps and permissions are left untouched. A conflicting or
  truncated payload fails before database work and cannot overwrite an original.
  Digests travel inside the archive, avoiding SSH argument limits for large clinics.
- Migration 0798's downgrade refuses after provenance/annotation metadata has
  been stored. Do not remove those columns to roll back application code.

Local verification only
-----------------------

From `system_CLIV4`: `go test ./...`.

From `system_importV4` (Node 24):

```
npm ci --ignore-scripts --no-audit --no-fund
node node_modules/typescript/bin/tsc --noEmit --incremental false
node node_modules/eslint/bin/eslint.js src/lib/photo-plan.ts src/lib/photo-import.ts src/lib/photo-run.ts src/lib/photo-publish.ts
npm run test:photos
```

The PostgreSQL test only accepts a disposable container with an explicit label.
It never reads a database URL or contacts an import server. Create a fresh local
container, set the two environment variables, then run the test. `PHOTO_IMPORT_V4_BACKEND`
must point to the matching V4 checkout containing migration 0798.

```
docker run --detach --rm --name drease-photo-import-test-local --label com.drease.photo-import-test=true --env POSTGRES_PASSWORD=photo-import-test-only postgres:16-alpine
```

PowerShell:

```
$env:PHOTO_IMPORT_TEST_CONTAINER = 'drease-photo-import-test-local'
$env:PHOTO_IMPORT_V4_BACKEND = '<absolute path to matching V4 backend checkout>'
node --experimental-strip-types --test scripts/photo-plan.postgres.test.mjs
docker stop drease-photo-import-test-local
```

The test applies actual V4 photo/face-chart migrations to synthetic foundational
tables. It verifies SQL execution, mixed functions on one day, multiple days,
multiple images with identical timestamps/sizes, reuse of a real encounter,
E5 filtering/versioning, preservation of existing chart ink and removed pages,
repeat imports, newer/older revisions, concurrent pin saves, signed visits,
metadata rollback refusal, latest-encounter selection, historical-path adoption,
placeholder-visit repair, repeated repair, cross-tenant preservation, and
missing-patient rejection. It does not assert that the
entire production migration chain or a live deployment was exercised.

The companion V4 backend integration tests exercise the actual HTTP GET/PUT media
handlers after its full migration chain on a separate disposable PostgreSQL
container, including stale/cross-visit E5 payload rejection and original photo pins.

Local PostgreSQL alternative (Windows, no Docker)
------------------------------------------------

Initialize a fresh private cluster whose final directory name starts with
`photo-import-test-`; bind it only to 127.0.0.1 on an unused port. The test checks
`SHOW data_directory` before resetting its synthetic schema. Never point it at
an existing development or shared cluster. Set:

```
$env:PHOTO_IMPORT_TEST_LOCAL_PORT = '<dedicated cluster port>'
$env:PHOTO_IMPORT_TEST_PSQL = '<absolute path to psql.exe>'
$env:PHOTO_IMPORT_V4_BACKEND = '<matching V4 checkout with provenance migration>'
node --experimental-strip-types --test scripts/photo-plan.postgres.test.mjs
```

Verification on 2 Nov 2026 used a disposable PostgreSQL 18 cluster on loopback.
Go tests and vet, TypeScript, ESLint, SQL execution and file-publication tests
passed. The existing backend provenance/media-separation change is a companion
dependency; no deployment or shared patient-data import was performed.

V4demo rollout
--------------

`PHOTO_IMPORT_V2_TARGETS=test` enables this path only for the configured test target.
Production runs the unchanged 48da457 importer (`photo-run-legacy.ts` and
`photo-import-legacy.ts`). `IMPORT_PHOTO_CLI_PATH` selects the matching V2 CLI;
`IMPORT_CLI_PATH` stays on the deployed original for production and other groups.
Do not add production to the rollout without an explicit production release.
The provenance migration was renumbered to 0798 after current master/CampDev.
