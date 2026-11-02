import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync, statSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, basename } from 'node:path';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { PHOTO_PUBLISH_SCRIPT } from '../src/lib/photo-publish.ts';

function archive(entries,expected=manifest(entries)) {
  const result=spawnSync('python',['-c',`import io,json,sys,tarfile
with tarfile.open(fileobj=sys.stdout.buffer,mode='w|gz') as tar:
 for name,data in json.load(sys.stdin).items():
  body=data.encode(); info=tarfile.TarInfo(name); info.size=len(body); tar.addfile(info,io.BytesIO(body))
`],{input:JSON.stringify({'.photo-import-manifest.json':JSON.stringify(expected),...entries}),windowsHide:true});
  assert.equal(result.status,0,result.stderr?.toString());
  return result.stdout;
}
const manifest=entries=>Object.fromEntries(Object.entries(entries).map(([name,data])=>[name,
  {size:Buffer.byteLength(data),sha256:createHash('sha256').update(data).digest('hex')}]));
function publish(root,entries,bytes=archive(entries)) {
  return spawnSync('python',['-c',PHOTO_PUBLISH_SCRIPT,root],{input:bytes,windowsHide:true});
}
function fixture(t) {
  const root=mkdtempSync(join(tmpdir(),'drease-photo-publish-test-'));
  t.after(()=>{
    assert.equal(dirname(root),tmpdir());
    assert.ok(basename(root).startsWith('drease-photo-publish-test-'));
    rmSync(root,{recursive:true,force:true});
  });
  return root;
}
test('publishes complete images and thumbnails; identical repeat never touches originals',t=>{
  const root=fixture(t), entries={'TEST/opd-photo/a.jpg':'photo','TEST/opd-photo-thumb/a.jpg':'thumb'};
  let result=publish(root,entries);
  assert.equal(result.status,0,result.stderr.toString());
  assert.deepEqual(JSON.parse(result.stdout),{created:2,existing:0});
  const original=statSync(join(root,'TEST/opd-photo/a.jpg'));
  result=publish(root,entries);
  assert.equal(result.status,0,result.stderr.toString());
  assert.deepEqual(JSON.parse(result.stdout),{created:0,existing:2});
  assert.equal(statSync(join(root,'TEST/opd-photo/a.jpg')).mtimeMs,original.mtimeMs);
});
test('different bytes at an existing path fail without overwriting any original or publishing new files',t=>{
  const root=fixture(t); mkdirSync(join(root,'TEST/opd-photo'),{recursive:true});
  const path=join(root,'TEST/opd-photo/a.jpg'); writeFileSync(path,'original-doctor-image');
  const result=publish(root,{'TEST/opd-photo/new.jpg':'new','TEST/opd-photo/a.jpg':'replacement'});
  assert.notEqual(result.status,0);
  assert.equal(readFileSync(path,'utf8'),'original-doctor-image');
  assert.equal(existsSync(join(root,'TEST/opd-photo/new.jpg')),false);
});
test('missing payload, wrong digest and unexpected archive paths do not publish partial images',t=>{
  const root=fixture(t), entries={'TEST/opd-photo/a.jpg':'photo','TEST/opd-photo/b.jpg':'second'};
  for(const bytes of [archive({'TEST/opd-photo/a.jpg':'photo'},manifest(entries)),archive({...entries,'../outside.jpg':'escape'},manifest(entries)),
    archive({'TEST/opd-photo/a.jpg':'wrong','TEST/opd-photo/b.jpg':'second'},manifest(entries))]) {
    const result=publish(root,entries,bytes);
    assert.notEqual(result.status,0);
    assert.equal(existsSync(join(root,'TEST/opd-photo/a.jpg')),false);
  }
});
