// Which legacy account becomes the clinic owner, picked from the codes the branch survey
// found in the users table (survey.owner_candidates).
//
// The owner's rules, 7 Oct 2026:
//
//   the first of admin → ease → sorawit that the file has wins          (priority as asked)
//   2ก  "found" means the WHOLE code, case ignored: Admin counts, admin2 and administrator
//       do not — the same comparison -owner makes (strings.EqualFold in membership.go)
//   3ก  none of the three → nothing is picked, and the form lists the codes the file has
//
// The code is returned as the dump spells it, so the field shows what the file says.

export const OWNER_PRIORITY = ["admin", "ease", "sorawit"] as const;

/** The highest-priority code present, as spelled in the dump, or "" when none is. */
export function pickOwnerUsername(candidates: readonly string[] | undefined): string {
  if (!candidates || candidates.length === 0) return "";
  for (const want of OWNER_PRIORITY) {
    const hit = candidates.find((c) => c.trim().toLowerCase() === want);
    if (hit !== undefined) return hit.trim();
  }
  return "";
}

/** How many candidate codes the form lists when nothing was picked. */
export const OWNER_CANDIDATES_SHOWN = 20;
