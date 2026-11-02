import {test} from 'node:test';
import assert from 'node:assert/strict';
import {usesPhotoManifestV2} from '../src/lib/photo-rollout.ts';
test('v4demo rollout leaves production on the original importer',()=>{
  assert.equal(usesPhotoManifestV2('test'),true);
  assert.equal(usesPhotoManifestV2('production'),false);
  assert.equal(usesPhotoManifestV2('test',''),false);
  assert.equal(usesPhotoManifestV2('production','test'),false);
  assert.equal(usesPhotoManifestV2('unknown','unknown'),false);
  assert.equal(usesPhotoManifestV2('production','test, production'),true);
});
