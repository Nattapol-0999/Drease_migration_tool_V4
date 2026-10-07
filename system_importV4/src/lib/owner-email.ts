// The owner login a clinic is created with, derived from the clinic name the operator types.
//
// Every clinic the team has created uses the same shape — cs+master-clinic@atiz.com,
// cs+pinluck-clinic@atiz.com — so typing it by hand was pure repetition and the place a typo
// slips into somebody's login. The owner chose the rules on 7 Oct 2026:
//
//   1ข  the name is used exactly as typed, nothing converted (case, spaces, Thai mixed in)
//   2ก  a name with no Latin letter or digit at all — empty, or Thai only — gives NO email,
//       because nothing in it can stand in an address; the form says why
//
// Only the ends are trimmed: the clinic name itself is sent trimmed (thaName: .trim()), and
// the email should read the same name that will be saved.

export const OWNER_EMAIL_PREFIX = "cs+";
export const OWNER_EMAIL_DOMAIN = "@atiz.com";

/** cs+<clinic name>@atiz.com, or "" when the name has nothing an address can carry. */
export function ownerEmailFromClinicName(name: string): string {
  const n = name.trim();
  if (!/[A-Za-z0-9]/.test(n)) return "";
  return `${OWNER_EMAIL_PREFIX}${n}${OWNER_EMAIL_DOMAIN}`;
}
