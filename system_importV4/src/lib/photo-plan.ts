import type { Manifest, Survey } from './photo-import';

const lit = (s: string): string => "'" + s.replaceAll("'", "''") + "'";
const optional = (s?: string): string => s?.trim() ? lit(s) : 'NULL';
const sourceTypes = new Set(['before_after', 'form', 'drawing', 'unknown']);

/** Refuse old CLI output: it has neither the true examination date nor provenance. */
export function validatePhotoManifest(manifest: Manifest, clinic: string): void {
  if (manifest.version !== 2 || manifest.clinic_code !== clinic) {
    throw new Error('ต้องใช้ photo manifest version 2 ของคลินิกนี้ — สร้างด้วย CLI รุ่นใหม่');
  }
  const refs = new Set<string>();
  const paths = new Set<string>();
  for (const f of manifest.files) {
    const parsed = new Date(`${f.visit_date}T00:00:00Z`);
    if (!/^\d{4}-\d{2}-\d{2}$/.test(f.visit_date) || !Number.isFinite(parsed.getTime()) ||
        parsed.toISOString().slice(0, 10) !== f.visit_date || Number(f.visit_date.slice(0, 4)) < 1900 ||
        Number(f.visit_date.slice(0, 4)) > 2200) {
      throw new Error(`วันที่ตรวจไม่ถูกต้อง: ${f.visit_date}`);
    }
    if (!f.hn?.trim() || f.clinic_code !== clinic || !f.source_ref?.startsWith('mris.page:') ||
        !sourceTypes.has(f.source_type) || !f.source_metadata || typeof f.source_metadata !== 'object' ||
        Array.isArray(f.source_metadata) || !Number.isFinite(Date.parse(f.taken_at))) {
      throw new Error('manifest ขาดวันที่ เวลา ประเภท หรือข้อมูลอ้างอิงต้นทางที่ถูกต้อง');
    }
    if (!['before', 'after', 'note'].includes(f.kind ?? '') ||
        (f.kind !== 'note' && (f.source_type !== 'before_after' || f.area_tag !== f.kind))) {
      throw new Error('ประเภท Before–After ไม่สอดคล้องกับที่มาของภาพ');
    }
    const key = JSON.stringify([f.hn, f.source_ref]);
    if (refs.has(key) || paths.has(f.target_path)) throw new Error('manifest มีหน้าเอกสารหรือไฟล์ซ้ำ');
    refs.add(key);
    paths.add(f.target_path);
  }
}

/** Generate reviewable SQL only. No connections, copies or application happen here. */
export function buildInsertSQL(clinic: string, manifest: Manifest, survey: Survey): { sql: string; rows: number } {
  validatePhotoManifest(manifest, clinic);
  const byPath = new Map(manifest.files.map(f => [f.target_path, f]));
  const matched = new Set<string>();
  const rows = survey.matched.map(m => {
    const f = byPath.get(m.target_path);
    if (!f || m.hn !== f.hn || m.source_url !== f.source_url || matched.has(m.target_path) ||
        !Number.isSafeInteger(m.bytes) || m.bytes <= 0 || m.bytes > 2147483647 ||
        (m.thumb && m.thumb.target_path !== f.thumb_target_path)) {
      throw new Error('ผลสำรวจไฟล์ไม่ตรงกับ manifest หรือขนาดไฟล์ไม่ถูกต้อง');
    }
    matched.add(m.target_path);
    return { f, m };
  });
  const c = lit(clinic);
  const out = [
    '-- Dr.Ease V4 legacy photo import: actual examination dates, stable page identities.',
    '-- Exact historical file identities repair prior imports. Images must be copied before applying.',
    'BEGIN;',
    'SET LOCAL standard_conforming_strings = on;',
    `SET LOCAL app.clinic_code = ${c};`,
    `DO $guard$ BEGIN
      IF NOT EXISTS (SELECT 1 FROM clinic WHERE clinic_code = ${c}) THEN
        RAISE EXCEPTION 'clinic_code ไม่มีใน V4';
      END IF;
      IF (SELECT count(*) FROM information_schema.columns
           WHERE table_schema = 'public' AND table_name = 'photos'
             AND column_name IN ('legacy_import', 'legacy_source_ref', 'legacy_source_type', 'legacy_metadata')) <> 4 THEN
        RAISE EXCEPTION 'ต้องมี V4 migrations 0420 และ 0798 ก่อนนำเข้ารูป';
      END IF;
      IF to_regclass('public.opd_visit_deletion') IS NULL THEN RAISE EXCEPTION 'ต้องมี migration 0716 ก่อนนำเข้ารูป'; END IF;
    END $guard$;`,
    // Serializes visit-number allocation with concurrent app INSERTs as well as imports.
    // This is an operator-run import transaction, not part of an interactive request.
    'LOCK TABLE visit_master IN SHARE ROW EXCLUSIVE MODE;',
    `CREATE TEMP TABLE imported_photo_plan (
      hn text NOT NULL, source_ref text NOT NULL, visit_date date NOT NULL,
      source_type text NOT NULL, area_tag text NOT NULL, kind text NOT NULL,
      file_path text NOT NULL, thumb_path text, file_size integer NOT NULL,
      doctor_code text, note text, taken_at timestamptz NOT NULL, metadata jsonb NOT NULL,
      PRIMARY KEY (hn, source_ref)
    ) ON COMMIT DROP;`,
  ];
  for (const { f, m } of rows) {
    out.push(`INSERT INTO imported_photo_plan VALUES (
      ${lit(f.hn)}, ${lit(f.source_ref)}, DATE ${lit(f.visit_date)}, ${lit(f.source_type)},
      ${lit(f.area_tag)}, ${lit(f.kind!)}, ${lit(f.target_path)}, ${optional(m.thumb?.target_path)}, ${m.bytes},
      ${optional(f.doctor_code)}, ${optional(f.note)}, ${lit(f.taken_at)}::timestamptz,
      ${lit(JSON.stringify(f.source_metadata))}::jsonb
    );`);
  }
  // Follow the same visit-row lock used by V4 chart, media and photo-pin saves.
  // Lock BEFORE inspection, so a doctor cannot add ink between preflight and upsert.
  out.push(`SELECT v.id FROM visit_master v
    WHERE v.clinic_code = ${c} AND EXISTS (SELECT 1 FROM imported_photo_plan s
      WHERE s.hn = v."HN" AND (s.visit_date = v.visit_date OR EXISTS (
        SELECT 1 FROM photos p WHERE p.clinic_code = ${c} AND p.hn = s.hn AND p.visit_id = v.id
          AND p.legacy_import AND (p.legacy_source_ref = s.source_ref OR (p.legacy_source_ref IS NULL AND
            (p.file_path = s.file_path OR s.metadata->'_legacy_file_paths' ? p.file_path))))))
    ORDER BY v.id FOR UPDATE;
    SELECT p.id FROM photos p JOIN imported_photo_plan s
      ON s.hn = p.hn AND (s.source_ref = p.legacy_source_ref OR (p.legacy_source_ref IS NULL AND
        (p.file_path = s.file_path OR s.metadata->'_legacy_file_paths' ? p.file_path)))
    WHERE p.clinic_code = ${c} AND p.legacy_import ORDER BY p.id FOR UPDATE OF p;`);
  // A stale source dump cannot roll a newer source page back to a former encounter.
  out.push(`DELETE FROM imported_photo_plan s USING photos p
    WHERE p.clinic_code = ${c} AND p.hn = s.hn AND p.legacy_import
      AND p.legacy_source_ref = s.source_ref AND p.taken_at > s.taken_at;`);
  out.push(`DO $preflight$ BEGIN
    IF EXISTS (SELECT 1 FROM imported_photo_plan s LEFT JOIN patient p
      ON p.clinic_code = ${c} AND p."HN" = s.hn
      WHERE p.id IS NULL OR coalesce(p.branch_code, '') = '') THEN
      RAISE EXCEPTION 'ข้อมูลคนไข้หรือสาขาไม่ครบ — ยังไม่สร้าง Visit หรือภาพที่ไม่มี Visit';
    END IF;
    IF EXISTS (SELECT 1 FROM photos p JOIN imported_photo_plan s
      ON s.hn = p.hn AND s.source_ref = p.legacy_source_ref
      LEFT JOIN visit_master v ON v.id = p.visit_id
      WHERE p.clinic_code = ${c} AND p.legacy_import AND
        (v.clinic_code <> ${c} OR v."HN" <> s.hn
         OR (p.taken_at = s.taken_at AND p.file_path <> s.file_path))) THEN
      RAISE EXCEPTION 'ข้อมูลหน้าเดิมขัดแย้งกับ manifest — ต้องตรวจสอบก่อนเปลี่ยน Visit/ประเภท/revision';
    END IF;
    IF EXISTS (SELECT 1 FROM photos p JOIN imported_photo_plan s
      ON s.hn = p.hn AND s.source_ref = p.legacy_source_ref
      WHERE p.clinic_code = ${c} AND p.legacy_import AND s.taken_at > p.taken_at AND
        (p.deleted_at IS NOT NULL OR EXISTS (SELECT 1 FROM visit_master v WHERE v.id = p.visit_id AND v.signed_at IS NOT NULL)
         OR coalesce(p.annotations, '[]'::jsonb) <> '[]'::jsonb
         OR p.updated_at IS DISTINCT FROM (p.legacy_metadata->>'_v4_imported_at')::timestamptz
         OR EXISTS (SELECT 1 FROM face_chart fc, jsonb_array_elements(fc.strokes) mark
           WHERE fc.clinic_code = ${c} AND fc.visit_id = p.visit_id
             AND (mark->>'view' = 'photo:' || p.id::text OR fc.base_type = 'photo:' || p.id::text)
             AND coalesce(mark->>'page', 'false') <> 'true')
         OR EXISTS (SELECT 1 FROM face_chart fc JOIN face_chart_pin pin ON pin.face_chart_id = fc.id
           WHERE fc.clinic_code = ${c} AND fc.visit_id = p.visit_id))) THEN
      RAISE EXCEPTION 'หน้าเดิมถูกแก้ไขใน V4 — ไม่แทนที่ภาพหรือเส้นวาดโดยอัตโนมัติ';
    END IF;
  END $preflight$;`);

  // Resolve the latest open encounter once for every downstream operation.
  out.push(`CREATE TEMP TABLE imported_photo_visits (
    hn text, visit_date date, visit_id bigint NOT NULL, PRIMARY KEY (hn, visit_date)
  ) ON COMMIT DROP;`);
  out.push(`DO $visits$ DECLARE r record; next_vn bigint; BEGIN
    FOR r IN SELECT DISTINCT ON (s.hn, s.visit_date) s.hn, s.visit_date, s.doctor_code, p.branch_code
      FROM imported_photo_plan s JOIN patient p ON p.clinic_code = ${c} AND p."HN" = s.hn
      ORDER BY s.hn, s.visit_date, s.taken_at, s.source_ref
    LOOP
      IF NOT EXISTS (SELECT 1 FROM visit_master v WHERE v.clinic_code = ${c}
        AND v."HN" = r.hn AND v.visit_date = r.visit_date AND v.signed_at IS NULL
        AND NOT EXISTS (SELECT 1 FROM opd_visit_deletion d WHERE d.clinic_code = ${c} AND d.visit_id = v.id)) THEN
        SELECT coalesce(max(CASE WHEN "VN" ~ '^[0-9]{1,15}$' THEN "VN"::bigint END), 0) + 1
          INTO next_vn FROM (SELECT "VN" FROM visit_master WHERE clinic_code = ${c} AND visit_date = r.visit_date
            UNION ALL SELECT vn AS "VN" FROM opd_visit_deletion WHERE clinic_code = ${c} AND visit_date = r.visit_date) all_vns;
        INSERT INTO visit_master (clinic_code, "HN", "VN", visit_date, branch_code, doctor_code, title)
        VALUES (${c}, r.hn, CASE WHEN next_vn <= 999 THEN lpad(next_vn::text, 3, '0') ELSE next_vn::text END,
          r.visit_date, r.branch_code, r.doctor_code, 'นำเข้าจากระบบเดิม');
      END IF;
      INSERT INTO imported_photo_visits
        SELECT r.hn, r.visit_date, v.id FROM visit_master v
        WHERE v.clinic_code = ${c} AND v."HN" = r.hn AND v.visit_date = r.visit_date
          AND v.signed_at IS NULL
          AND NOT EXISTS (SELECT 1 FROM opd_visit_deletion d WHERE d.clinic_code = ${c} AND d.visit_id = v.id)
        ORDER BY v.created_at DESC, v.id DESC LIMIT 1;
    END LOOP;
  END $visits$;`);

  // Exact revision-derived paths identify older imports, never timestamp/size.
  out.push(`CREATE TEMP TABLE imported_photo_repair ON COMMIT DROP AS
    SELECT p.id, p.visit_id AS old_visit_id, s.hn, s.source_ref, s.visit_date, s.source_type,
      s.area_tag, s.kind, s.metadata, mv.visit_id AS new_visit_id,
      p.legacy_source_ref IS NULL AS adopted,
      (p.legacy_source_type IS DISTINCT FROM s.source_type OR p.kind IS DISTINCT FROM s.kind OR p.area_tag IS DISTINCT FROM s.area_tag) AS reclassified
    FROM photos p JOIN imported_photo_plan s ON s.hn = p.hn AND (
      s.source_ref = p.legacy_source_ref OR (p.legacy_source_ref IS NULL AND
        (p.file_path = s.file_path OR s.metadata->'_legacy_file_paths' ? p.file_path)))
    JOIN imported_photo_visits mv ON mv.hn = s.hn AND mv.visit_date = s.visit_date
    WHERE p.clinic_code = ${c} AND p.legacy_import;
    DO $repair$ BEGIN
      IF EXISTS (SELECT id FROM imported_photo_repair GROUP BY id HAVING count(*) > 1)
        OR EXISTS (SELECT hn, source_ref FROM imported_photo_repair GROUP BY hn, source_ref HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'ไฟล์เก่าจับคู่หลายหน้า — ต้องตรวจสอบ ไม่รวมภาพอัตโนมัติ';
      END IF;
      IF EXISTS (SELECT 1 FROM imported_photo_repair r JOIN photos p ON p.id = r.id
        LEFT JOIN visit_master v ON v.id = p.visit_id
        WHERE (r.adopted OR r.old_visit_id IS DISTINCT FROM r.new_visit_id
          OR p.legacy_source_type IS DISTINCT FROM r.source_type
          OR p.area_tag IS DISTINCT FROM r.area_tag OR p.kind IS DISTINCT FROM r.kind)
        AND (p.deleted_at IS NOT NULL OR v.signed_at IS NOT NULL
          OR EXISTS (SELECT 1 FROM opd_visit_deletion d WHERE d.clinic_code = ${c} AND d.visit_id = p.visit_id)
          OR coalesce(p.annotations, '[]'::jsonb) <> '[]'::jsonb
          OR (r.adopted AND p.updated_at IS DISTINCT FROM p.created_at)
          OR (NOT r.adopted AND p.updated_at IS DISTINCT FROM (p.legacy_metadata->>'_v4_imported_at')::timestamptz)
          OR EXISTS (SELECT 1 FROM face_chart fc, jsonb_array_elements(fc.strokes) mark
            WHERE fc.clinic_code = ${c} AND fc.visit_id = p.visit_id AND coalesce(mark->>'page', 'false') <> 'true')
          OR EXISTS (SELECT 1 FROM face_chart fc JOIN face_chart_pin pin ON pin.face_chart_id = fc.id
            WHERE fc.clinic_code = ${c} AND fc.visit_id = p.visit_id))) THEN
        RAISE EXCEPTION 'ข้อมูลเดิมถูกแก้ไข ลงนาม หรือลบแล้ว — ไม่ย้าย/เปลี่ยนประเภทอัตโนมัติ';
      END IF;
    END $repair$;`);
  out.push(`UPDATE photos p SET visit_id = r.new_visit_id,
      branch_code = v.branch_code, legacy_source_ref = r.source_ref,
      legacy_source_type = r.source_type, area_tag = r.area_tag, kind = r.kind,
      legacy_metadata = r.metadata || jsonb_build_object('_v4_imported_at', now(),
        '_v4_previous_visit_id', r.old_visit_id, '_v4_adopted_legacy', r.adopted), updated_at = now()
    FROM imported_photo_repair r JOIN visit_master v ON v.id = r.new_visit_id
    WHERE p.id = r.id AND (r.adopted OR r.old_visit_id IS DISTINCT FROM r.new_visit_id
      OR p.legacy_source_type IS DISTINCT FROM r.source_type
      OR p.area_tag IS DISTINCT FROM r.area_tag OR p.kind IS DISTINCT FROM r.kind);
    UPDATE face_chart fc SET strokes = q.strokes,
      base_type = CASE WHEN EXISTS (SELECT 1 FROM imported_photo_repair r
        WHERE r.old_visit_id = fc.visit_id AND fc.base_type = 'photo:' || r.id::text
          AND (r.old_visit_id IS DISTINCT FROM r.new_visit_id OR r.source_type NOT IN ('form','drawing')))
        THEN coalesce(q.strokes->0->>'view', 'blank') ELSE fc.base_type END,
      version = fc.version + 1, updated_at = now()
    FROM (SELECT old.id, coalesce(jsonb_agg(mark ORDER BY ord)
      FILTER (WHERE mark IS NOT NULL AND NOT EXISTS (SELECT 1 FROM imported_photo_repair r
        WHERE r.old_visit_id = old.visit_id AND mark->>'view' = 'photo:' || r.id::text
          AND (r.old_visit_id IS DISTINCT FROM r.new_visit_id OR r.source_type NOT IN ('form','drawing')))), '[]'::jsonb) AS strokes
      FROM face_chart old LEFT JOIN LATERAL jsonb_array_elements(old.strokes) WITH ORDINALITY marks(mark,ord) ON true
      WHERE old.clinic_code = ${c} AND EXISTS (SELECT 1 FROM imported_photo_repair r
        WHERE r.old_visit_id = old.visit_id AND (r.adopted OR r.old_visit_id IS DISTINCT FROM r.new_visit_id OR r.reclassified))
      GROUP BY old.id) q
    WHERE fc.id = q.id AND fc.strokes IS DISTINCT FROM q.strokes;`);

  // Only brand-new source pages may add E1 markers. An exact repeat must not
  // restore a page a clinician deliberately removed from an existing chart.
  out.push(`CREATE TEMP TABLE imported_photo_new_pages ON COMMIT DROP AS
    SELECT s.hn, s.source_ref FROM imported_photo_plan s WHERE EXISTS (
      SELECT 1 FROM imported_photo_repair r WHERE r.hn = s.hn AND r.source_ref = s.source_ref
        AND (r.adopted OR r.reclassified OR r.old_visit_id IS DISTINCT FROM r.new_visit_id)) OR NOT EXISTS (
      SELECT 1 FROM photos p WHERE p.clinic_code = ${c} AND p.hn = s.hn
        AND p.legacy_import AND p.legacy_source_ref = s.source_ref);`);
  out.push(`CREATE TEMP TABLE imported_photo_changes ON COMMIT DROP AS WITH changed AS (
    INSERT INTO photos (clinic_code, hn, branch_code, visit_id, area_tag, kind, file_path,
      thumb_path, file_size, consent_given, note, doctor_code, taken_at, annotations,
      legacy_import, legacy_source_ref, legacy_source_type, legacy_metadata, created_at, updated_at)
    SELECT ${c}, s.hn, v.branch_code, v.id, s.area_tag, s.kind, s.file_path, s.thumb_path,
      s.file_size, true, s.note, s.doctor_code, s.taken_at, '[]'::jsonb, true, s.source_ref,
      s.source_type, s.metadata || jsonb_build_object('_v4_imported_at', now()), now(), now()
    FROM imported_photo_plan s JOIN imported_photo_visits mv ON mv.hn = s.hn AND mv.visit_date = s.visit_date
    JOIN visit_master v ON v.id = mv.visit_id
    ORDER BY s.taken_at, s.hn, s.source_ref
    ON CONFLICT (clinic_code, hn, legacy_source_ref) WHERE legacy_import AND legacy_source_ref IS NOT NULL
    DO UPDATE SET file_path = EXCLUDED.file_path, thumb_path = EXCLUDED.thumb_path,
      file_size = EXCLUDED.file_size, taken_at = EXCLUDED.taken_at, note = EXCLUDED.note,
      legacy_metadata = photos.legacy_metadata || EXCLUDED.legacy_metadata, updated_at = EXCLUDED.updated_at
    WHERE photos.taken_at < EXCLUDED.taken_at
    RETURNING visit_id, legacy_source_type
    ) SELECT * FROM changed;`);

  // Invalidate stale E5 editors after a real media change; exact repeats do not
  // change versions. E1's chart upsert below already advances its own version.
  out.push(`INSERT INTO chart_engine_version (clinic_code, visit_id, engine, version, updated_at)
    SELECT DISTINCT ${c}, visit_id, 'media', 1, now() FROM imported_photo_changes
      WHERE legacy_source_type = 'before_after'
    UNION SELECT ${c}, old_visit_id, 'media', 1, now() FROM imported_photo_repair
      WHERE old_visit_id IS NOT NULL AND (adopted OR reclassified OR old_visit_id IS DISTINCT FROM new_visit_id)
    UNION SELECT ${c}, new_visit_id, 'media', 1, now() FROM imported_photo_repair
      WHERE adopted OR reclassified OR old_visit_id IS DISTINCT FROM new_visit_id
    ON CONFLICT (clinic_code, visit_id, engine) DO UPDATE
      SET version = chart_engine_version.version + 1, updated_at = now();`);

  // Only record forms/drawings become E1 pages. Before–After never becomes a drawing page.
  // Add page markers without replacing existing V4 ink, pins or the selected base page.
  out.push(`INSERT INTO face_chart (clinic_code, "HN", visit_id, base_type, doctor_code, strokes, version)
    SELECT ${c}, v."HN", v.id, 'photo:' || (array_agg(p.id ORDER BY p.taken_at DESC, p.id DESC))[1]::text,
      v.doctor_code, jsonb_agg(jsonb_build_object('page', true, 'view', 'photo:' || p.id::text)
        ORDER BY p.taken_at, p.id), 1
    FROM visit_master v JOIN photos p ON p.visit_id = v.id AND p.clinic_code = v.clinic_code AND p.hn = v."HN"
    JOIN imported_photo_new_pages n ON n.hn = p.hn AND n.source_ref = p.legacy_source_ref
    WHERE v.clinic_code = ${c} AND p.legacy_import AND p.deleted_at IS NULL
      AND p.legacy_source_type IN ('form', 'drawing')
      AND EXISTS (SELECT 1 FROM imported_photo_plan s WHERE s.hn = v."HN" AND s.visit_date = v.visit_date)
    GROUP BY v.id, v."HN", v.doctor_code
    ON CONFLICT (clinic_code, visit_id) DO UPDATE
    SET strokes = face_chart.strokes || (SELECT jsonb_agg(mark) FROM jsonb_array_elements(EXCLUDED.strokes) mark
      WHERE NOT EXISTS (SELECT 1 FROM jsonb_array_elements(face_chart.strokes) old WHERE old->>'view' = mark->>'view')),
      version = face_chart.version + 1, updated_at = now()
    WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(EXCLUDED.strokes) mark
      WHERE NOT EXISTS (SELECT 1 FROM jsonb_array_elements(face_chart.strokes) old WHERE old->>'view' = mark->>'view'));`);
  out.push(`SELECT s.visit_date, s.source_type, s.kind, count(*) AS planned_images
    FROM imported_photo_plan s GROUP BY s.visit_date, s.source_type, s.kind
    ORDER BY s.visit_date, s.source_type, s.kind;`, 'COMMIT;');
  return { sql: out.join('\n\n') + '\n', rows: rows.length };
}
