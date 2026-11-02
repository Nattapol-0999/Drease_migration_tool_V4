import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { spawnSync, spawn } from 'node:child_process';
import { photo, plan } from './photo-plan.test.mjs';

const container = process.env.PHOTO_IMPORT_TEST_CONTAINER;
const backend = process.env.PHOTO_IMPORT_V4_BACKEND;
const localPort = process.env.PHOTO_IMPORT_TEST_LOCAL_PORT;
const psql = process.env.PHOTO_IMPORT_TEST_PSQL || 'psql';
const sqlCommand = localPort ? [psql, ['-h','127.0.0.1','-p',localPort,'-U','postgres','-d','postgres','-v','ON_ERROR_STOP=1','-At']]
  : ['docker',['exec','-i',container,'psql','-U','postgres','-v','ON_ERROR_STOP=1','-At']];
function sql(text, expectSuccess=true) {
  const p=spawnSync(...sqlCommand, {input:text,encoding:'utf8',windowsHide:true});
  if(expectSuccess) assert.equal(p.status,0,p.stderr);
  return p;
}
test('V4 PostgreSQL: actual visits, E1/E5 separation, repeatability and protection of user drawings',
  {skip: (!container && !localPort) || !backend}, async () => {
  if (localPort) {
    assert.match(localPort,/^\d+$/);
    assert.match(sql('SHOW data_directory;').stdout.trim(), /[\\/]photo-import-test-[^\\/]+$/,
      'local tests require a dedicated cluster named photo-import-test-*');
  } else {
    assert.match(container,/^drease-photo-import-test-/);
    const label=spawnSync('docker',['inspect','--format','{{index .Config.Labels "com.drease.photo-import-test"}}',container],{encoding:'utf8',windowsHide:true});
    assert.equal(label.status,0,label.stderr);
    assert.equal(label.stdout.trim(),'true');
  }
  const migration = name => readFileSync(join(backend,'internal/db/migrations',name),'utf8');
  sql(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;
    CREATE TABLE clinic (clinic_code text PRIMARY KEY);
    CREATE TABLE patient (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,clinic_code text,"HN" text,branch_code text, UNIQUE(clinic_code,"HN"));
    CREATE TABLE visit_master (id int GENERATED ALWAYS AS IDENTITY PRIMARY KEY,clinic_code text,"HN" text,"VN" text,
      created_at timestamptz DEFAULT now(),visit_date date, branch_code text NOT NULL CHECK(branch_code<>''),doctor_code text,title text,signed_at timestamptz,
      UNIQUE(clinic_code,visit_date,"VN"));
    ${migration('0031_create_v3_photos.up.sql')}
    ALTER TABLE photos ADD COLUMN clinic_code varchar(32);
    ${migration('0164_photos_visit_id.up.sql')}
    ${migration('0167_photo_annotations.up.sql')}
    ${migration('0420_photos_legacy_import.up.sql')}
    ${migration('0798_photos_legacy_provenance.up.sql')}
    ${migration('0162_create_face_chart.up.sql')}
    ${migration('0198_face_chart_strokes.up.sql')}
    ALTER TABLE face_chart ADD COLUMN version bigint NOT NULL DEFAULT 1;
    CREATE TABLE chart_engine_version (clinic_code text,visit_id bigint,engine text,version bigint,updated_at timestamptz,
      PRIMARY KEY(clinic_code,visit_id,engine));
    CREATE TABLE opd_visit_deletion (clinic_code text, visit_id bigint, visit_date date, vn text);
    INSERT INTO clinic VALUES ('TEST');
    INSERT INTO patient (clinic_code,"HN",branch_code) VALUES ('TEST','HN1','BR1');
    INSERT INTO visit_master (clinic_code,"HN","VN",visit_date,branch_code,title)
      VALUES ('TEST','HN1','099','2026-07-29','BR1','existing actual encounter');
    INSERT INTO face_chart (clinic_code,"HN",visit_id,base_type,strokes)
      VALUES ('TEST','HN1',1,'face-front','[{"view":"front","points":[[0.1,0.2]]}]');`);
  const files=[...Array.from({length:5},(_,i)=>photo(`before${i}`)),photo('after','before_after','2026-07-29','after'),
    photo('composite','before_after','2026-07-29','note'),photo('form','form','2026-07-29','note'),
    photo('drawing','drawing','2026-07-29','note'),photo('unknown','unknown','2026-07-29','note'),
    photo('nextday','before_after','2026-07-30')];
  sql(plan(files).sql);
  const values = text => JSON.parse(sql(`SELECT coalesce(jsonb_agg(q),'[]') FROM (${text}) q;`).stdout.trim());
  assert.equal(values('SELECT * FROM photos').length,11,'equal time and size do not collapse distinct pages');
  assert.equal(values('SELECT * FROM visit_master').length,2);
  assert.equal(values(`SELECT * FROM visit_master WHERE visit_date='2026-01-01'`).length,0);
  assert.equal(values(`SELECT * FROM visit_master WHERE id=1`)[0].title,'existing actual encounter');
  const chart=values('SELECT * FROM face_chart')[0];
  const mediaVersion=values(`SELECT * FROM chart_engine_version WHERE visit_id=1 AND engine='media'`)[0].version;
  assert.equal(chart.base_type,'face-front');
  assert.equal(chart.strokes.length,3);
  assert.deepEqual(chart.strokes[0],{view:'front',points:[[0.1,0.2]]});
  const ids=values(`SELECT id,legacy_source_type FROM photos WHERE visit_id=1`);
  for(const mark of chart.strokes.slice(1)) {
    assert.ok(['form','drawing'].includes(ids.find(p=>`photo:${p.id}`===mark.view)?.legacy_source_type));
  }
  const query=readFileSync(join(backend,'internal/db/query/chart_media.sql'),'utf8')
    .split('-- name: ListMediaByVisit :many')[1].split(';')[0]
    .replace('sqlc.arg(clinic_code)',"'TEST'").replace('sqlc.arg(visit_id)','1');
  const media=values(query);
  assert.equal(media.length,7,'E5 contains five Before, one After, one composite; no forms/drawings/unknown');
  sql(plan(files).sql);
  assert.equal(values('SELECT * FROM photos').length,11);
  assert.equal(values(`SELECT * FROM chart_engine_version WHERE visit_id=1 AND engine='media'`)[0].version,mediaVersion);
  assert.equal(values('SELECT * FROM face_chart')[0].version,chart.version,'identical re-run leaves chart unchanged');
  const form=values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:form'`)[0];
  sql(`UPDATE face_chart SET strokes=(SELECT jsonb_agg(mark) FROM jsonb_array_elements(strokes) mark
    WHERE mark->>'view' <> 'photo:${form.id}') WHERE visit_id=1;`);
  const removedPageChart=values('SELECT * FROM face_chart')[0];
  sql(plan(files).sql);
  assert.deepEqual(values('SELECT * FROM face_chart')[0],removedPageChart,'repeat must not restore a page removed by the doctor');
  sql(`UPDATE face_chart SET strokes=strokes || '[{"view":"photo:${form.id}","points":[[0.3,0.4]]}]'::jsonb WHERE visit_id=1;`);
  assert.notEqual(sql(plan([{...files[7],taken_at:'2026-08-01T00:00:00Z',target_path:'TEST/opd-photo/form-new.jpg',source_url:'form-new.jpg'}]).sql,false).status,0,
    'a newer scan must not invalidate V4 ink drawn on the old scan');
  const original=values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:before0'`)[0];
  const newer={...files[0],taken_at:'2026-07-31T01:00:00Z',target_path:'TEST/opd-photo/newer.jpg',source_url:'newer.jpg'};
  sql(plan([newer]).sql);
  assert.equal(values(`SELECT * FROM chart_engine_version WHERE visit_id=1 AND engine='media'`)[0].version,mediaVersion+1);
  assert.equal(values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:before0'`)[0].id,original.id);
  sql(plan([files[0]]).sql);
  assert.equal(values(`SELECT * FROM photos WHERE id=${original.id}`)[0].file_path,newer.target_path,'old dump cannot regress latest image');
  sql(`UPDATE photos SET annotations='[{"x":0.1,"y":0.2,"label":"doctor"}]' WHERE id=${original.id};`);
  const conflicting={...newer,taken_at:'2026-08-01T00:00:00Z',target_path:'TEST/opd-photo/conflict.jpg',source_url:'conflict.jpg'};
  assert.notEqual(sql(plan([conflicting]).sql,false).status,0,'must not overwrite user annotations');
  assert.equal(values(`SELECT * FROM photos WHERE id=${original.id}`)[0].file_path,newer.target_path);
  sql(`INSERT INTO visit_master (clinic_code,"HN","VN",visit_date,branch_code) VALUES('TEST','HN1','100','2026-07-29','BR1');`);
  sql(plan([photo('latest')]).sql);
  const latest=values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:latest'`)[0];
  assert.equal(latest.visit_id,values(`SELECT id FROM visit_master WHERE "VN"='100' AND visit_date='2026-07-29'`)[0].id);
  const missing={...photo('missing'),hn:'MISSING'};
  assert.notEqual(sql(plan([missing]).sql,false).status,0);
  assert.equal(values(`SELECT * FROM visit_master WHERE "HN"='MISSING'`).length,0);
  sql(`INSERT INTO patient (clinic_code,"HN",branch_code) VALUES ('TEST','OLD','BR1');
    INSERT INTO visit_master (clinic_code,"HN","VN",visit_date,branch_code) VALUES ('TEST','OLD','001','2026-01-01','BR1');
    INSERT INTO photos (clinic_code,hn,visit_id,file_path,file_size,taken_at,legacy_import)
      SELECT 'TEST','OLD',id,'TEST/opd-photo/old-import.jpg',1234,'2026-07-30T01:00:00Z',true FROM visit_master WHERE "HN"='OLD';`);
  const old=values(`SELECT * FROM photos WHERE hn='OLD'`)[0];
  sql(`INSERT INTO face_chart (clinic_code,"HN",visit_id,base_type,strokes) VALUES
    ('TEST','OLD',${old.visit_id},'photo:${old.id}','[{"page":true,"view":"photo:${old.id}"}]');`);
  const oldPlan={...photo('old-import','form','2026-07-29','note'),hn:'OLD',
    source_metadata:{anno:[{original:'ink'}]}};
  sql(plan([oldPlan]).sql);
  const repaired=values(`SELECT * FROM photos WHERE hn='OLD'`)[0];
  assert.equal(repaired.id,old.id,'repair retains photo identity');
  assert.notEqual(repaired.visit_id,old.visit_id);
  assert.equal(repaired.legacy_source_type,'form');
  assert.deepEqual(repaired.legacy_metadata.anno,[{original:'ink'}]);
  assert.deepEqual(values(`SELECT strokes FROM face_chart WHERE visit_id=${old.visit_id}`)[0].strokes,[]);
  assert.deepEqual(values(`SELECT strokes FROM face_chart WHERE visit_id=${repaired.visit_id}`)[0].strokes,[{page:true,view:`photo:${old.id}`}]);
  sql(plan([oldPlan]).sql);
  assert.equal(values(`SELECT * FROM photos WHERE hn='OLD'`).length,1);
  assert.equal(values(`SELECT * FROM visit_master WHERE "HN"='OLD'`).length,2);
  sql(`UPDATE visit_master SET signed_at=now() WHERE visit_date='2026-07-30';`);
  sql(plan([photo('signed-new','before_after','2026-07-30')]).sql);
  const signedNew=values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:signed-new'`)[0];
  assert.equal(values(`SELECT signed_at FROM visit_master WHERE id=${signedNew.visit_id}`)[0].signed_at,null);
  // Exact historical paths can adopt an older scan, then advance the same photo id.
  sql(`INSERT INTO patient (clinic_code,"HN",branch_code) VALUES ('TEST','OLDER','BR1'),('TEST','EDITED','BR1'),('OTHER','OLD','BR2');
    INSERT INTO photos (clinic_code,hn,file_path,file_size,taken_at,legacy_import)
      VALUES ('TEST','OLDER','TEST/opd-photo/prior.jpg',1000,'2026-07-29T01:00:00Z',true),
             ('OTHER','OLD','TEST/opd-photo/old-import.jpg',1234,'2026-07-30T01:00:00Z',true),
             ('TEST','EDITED','TEST/opd-photo/edited.jpg',1234,'2026-07-30T01:00:00Z',true);
    UPDATE photos SET annotations='[{"x":0.1,"y":0.2,"label":"saved pin"}]' WHERE hn='EDITED';`);
  const olderId=values(`SELECT id FROM photos WHERE hn='OLDER'`)[0].id;
  sql(plan([{...photo('advanced','drawing','2026-07-29','note'),hn:'OLDER',
    source_metadata:{anno:[{kept:true}],_legacy_file_paths:['TEST/opd-photo/prior.jpg']}}]).sql);
  const advanced=values(`SELECT * FROM photos WHERE hn='OLDER'`)[0];
  assert.equal(advanced.id,olderId);
  assert.equal(advanced.file_path,'TEST/opd-photo/advanced.jpg');
  assert.equal(advanced.legacy_metadata._v4_adopted_legacy,true);
  assert.equal(values(`SELECT legacy_source_ref FROM photos WHERE clinic_code='OTHER' AND hn='OLD'`)[0].legacy_source_ref,null);
  const editedBefore=values(`SELECT * FROM photos WHERE hn='EDITED'`)[0];
  assert.notEqual(sql(plan([{...photo('edited'),hn:'EDITED'}]).sql,false).status,0);
  assert.deepEqual(values(`SELECT * FROM photos WHERE hn='EDITED'`)[0],editedBefore);
  // A second untouched import can be rebound to the latest visit on a later run.
  sql(`INSERT INTO patient (clinic_code,"HN",branch_code) VALUES ('TEST','REFILE','BR1');`);
  const refile={...photo('refile','form','2026-09-01','note'),hn:'REFILE'};
  sql(plan([refile]).sql);
  const refileBefore=values(`SELECT * FROM photos WHERE hn='REFILE'`)[0];
  sql(`INSERT INTO visit_master (clinic_code,"HN","VN",visit_date,branch_code,created_at)
    VALUES ('TEST','REFILE','002','2026-09-01','BR1',now()+interval '1 hour');`);
  sql(plan([refile]).sql);
  const refileAfter=values(`SELECT * FROM photos WHERE hn='REFILE'`)[0];
  assert.equal(refileBefore.id,refileAfter.id);
  assert.notEqual(refileBefore.visit_id,refileAfter.visit_id);
  assert.deepEqual(values(`SELECT strokes FROM face_chart WHERE visit_id=${refileBefore.visit_id}`)[0].strokes,[]);
  const stable=values(`SELECT * FROM photos WHERE hn='REFILE'`);
  sql(plan([refile]).sql);
  assert.deepEqual(values(`SELECT * FROM photos WHERE hn='REFILE'`),stable);
  // "Latest" means creation time, not the highest id; deleted visits never win.
  sql(`INSERT INTO visit_master (clinic_code,"HN","VN",visit_date,branch_code,created_at)
    VALUES ('TEST','REFILE','003','2026-09-01','BR1',now()-interval '1 day'),
           ('TEST','REFILE','004','2026-09-01','BR1',now()+interval '2 hours');
    INSERT INTO opd_visit_deletion (clinic_code,visit_id,visit_date,vn)
      SELECT 'TEST',id,visit_date,"VN" FROM visit_master WHERE "HN"='REFILE' AND "VN"='004';`);
  sql(plan([refile]).sql);
  assert.deepEqual(values(`SELECT * FROM photos WHERE hn='REFILE'`),stable);
  // Old dumps cannot refile a newer page into a different day or create an empty encounter.
  sql(plan([{...refile,visit_date:'2025-02-01',taken_at:'2026-07-29T01:00:00Z'}]).sql);
  assert.deepEqual(values(`SELECT * FROM photos WHERE hn='REFILE'`),stable);
  assert.equal(values(`SELECT id FROM visit_master WHERE "HN"='REFILE' AND visit_date='2025-02-01'`).length,0);
  // Reclassification invalidates stale E5 editors and removes the previous E1 page marker.
  sql(`INSERT INTO patient (clinic_code,"HN",branch_code) VALUES ('TEST','RECLASS','BR1');`);
  const classified={...photo('reclass','form','2026-09-02','note'),hn:'RECLASS'};
  sql(plan([classified]).sql);
  const classifiedRow=values(`SELECT * FROM photos WHERE hn='RECLASS'`)[0];
  sql(plan([{...classified,source_type:'before_after',kind:'before',area_tag:'before'}]).sql);
  assert.deepEqual(values(`SELECT strokes FROM face_chart WHERE visit_id=${classifiedRow.visit_id}`)[0].strokes,[]);
  assert.equal(values(`SELECT version FROM chart_engine_version WHERE visit_id=${classifiedRow.visit_id} AND engine='media'`)[0].version,1);
  // Signed source records cannot be moved even when a later open target exists.
  sql(`UPDATE visit_master SET signed_at=now() WHERE id=${refileAfter.visit_id};`);
  assert.notEqual(sql(plan([refile]).sql,false).status,0);
  assert.deepEqual(values(`SELECT * FROM photos WHERE hn='REFILE'`),stable);
  const beforeRollback=values('SELECT * FROM photos');
  assert.notEqual(sql('BEGIN;'+migration('0798_photos_legacy_provenance.down.sql')+'COMMIT;',false).status,0,
    'schema rollback must preserve source metadata after import');
  assert.deepEqual(values('SELECT * FROM photos'),beforeRollback);
  // An in-flight doctor save holds the same visit-row lock the importer takes.
  // Import must wait, then see the new pins and refuse to invalidate them.
  const racing=photo('race','before_after','2026-08-02');
  sql(plan([racing]).sql);
  const raceRow=values(`SELECT * FROM photos WHERE legacy_source_ref='mris.page:race'`)[0];
  const doctor=spawn(...sqlCommand,
    {stdio:['pipe','pipe','pipe'],windowsHide:true});
  const exited=new Promise(resolve=>doctor.on('exit',resolve));
  let stderr=''; doctor.stderr.on('data',b=>{stderr+=b.toString()});
  const locked=new Promise((resolve,reject)=>{
    let output='';
    doctor.stdout.on('data',b=>{output+=b.toString();if(output.includes('photo-review-lock-held'))resolve()});
    doctor.on('error',reject);
    doctor.on('exit',code=>{if(!output.includes('photo-review-lock-held'))reject(new Error(`doctor fixture failed ${code}: ${stderr}`))});
  });
  doctor.stdin.end(`BEGIN; SELECT id FROM visit_master WHERE id=${raceRow.visit_id} FOR UPDATE;
    UPDATE photos SET annotations='[{"x":0.3,"y":0.4,"label":"concurrent"}]',updated_at=now() WHERE id=${raceRow.id};
    SELECT 'photo-review-lock-held'; SELECT pg_sleep(1); COMMIT;`);
  await locked;
  const raceResult=sql(plan([{...racing,taken_at:'2026-08-03T01:00:00Z',target_path:'TEST/opd-photo/race-new.jpg',source_url:'race-new.jpg'}]).sql,false);
  assert.notEqual(raceResult.status,0,'concurrent pin save must win over image replacement');
  assert.equal(await exited,0,stderr);
  const preserved=values(`SELECT * FROM photos WHERE id=${raceRow.id}`)[0];
  assert.equal(preserved.file_path,racing.target_path);
  assert.equal(preserved.annotations[0].label,'concurrent');
});
