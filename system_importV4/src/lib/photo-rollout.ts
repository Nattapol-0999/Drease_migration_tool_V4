/** Server-owned rollout: default to v4demo/test. Unknown targets never enable V2. */
export function usesPhotoManifestV2(target: string, configured = "test"): boolean {
  return (target === "test" || target === "production") &&
    configured.split(",").map((value) => value.trim()).includes(target);
}
