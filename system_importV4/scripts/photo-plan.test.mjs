import assert from 'node:assert/strict';
import { test } from 'node:test';
import { buildInsertSQL, validatePhotoManifest } from '../src/lib/photo-plan.ts';

export function photo(id, source_type = 'before_after', day = '2026-07-29', kind = 'before') {
  return { hn: 'HN1', clinic_code: 'TEST', visit_date: day, source_type,
    source_ref: `mris.page:${id}`, source_metadata: { page_id: id, doctype: kind, vd_first: day },
    taken_at: '2026-07-30T01:00:00Z', kind, area_tag: kind,
    source_path: `nas-doctorease/${id}.jpg`, source_url: `${id}.jpg`, target_path: `TEST/opd-photo/${id}.jpg` };
}
export function plan(files) {
  return buildInsertSQL('TEST', { version: 2, clinic_code: 'TEST', uploads_subdir: 'TEST/opd-photo', files }, {
    matched: files.map(f => ({hn: f.hn, source_url:f.source_url, target_path:f.target_path, bytes:1234})),
  });
}

test('requires versioned actual-day provenance; old CLI cannot silently use a placeholder date', () => {
  assert.throws(() => validatePhotoManifest({clinic_code:'TEST',files:[]},'TEST'), /version 2/);
  for (const day of ['', '2026-02-30', '2026-00-00', '0001-01-01']) {
    assert.throws(() => plan([photo('p1','before_after',day)]), /วันที่ตรวจ/);
  }
});
test('retains separate pages with identical revision timestamp and file size', () => {
  const result = plan([photo('one'),photo('two')]);
  assert.equal(result.rows,2);
  assert.match(result.sql,/mris.page:one/);
  assert.match(result.sql,/mris.page:two/);
  assert.doesNotMatch(result.sql,/IMPORT_VISIT_DATE|2026-01-01/);
});
test('rejects duplicate page identities, mismatched type, and cross-tenant input', () => {
  assert.throws(() => plan([photo('one'),photo('one')]), /ซ้ำ/);
  assert.throws(() => plan([photo('one','form')]), /Before/);
  assert.throws(() => plan([{...photo('one'),clinic_code:'OTHER'}]), /manifest/);
});
test('escapes source text as SQL literals, not comments or executable SQL', () => {
  const result = plan([{...photo('one'),note:"test'); DROP TABLE photos; --\nnext"}]);
  assert.match(result.sql,/'test''\); DROP TABLE photos; --\nnext'/);
});
