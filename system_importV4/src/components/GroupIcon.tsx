import type { ImportGroup } from "@/lib/types";

/**
 * GroupIcon draws the mark for one import group.
 *
 * WHY THESE ARE HAND-DRAWN AND NOT A LIBRARY
 *
 * The tool has four runtime dependencies (next, react, react-dom, ssh2). An icon set adds a
 * fifth for eight glyphs that never change, and the ones that matter here — a receipt, a
 * stock lot, a points balance — are not in a generic set anyway. Inline SVG also inherits
 * `currentColor`, so these follow the light/dark theme with no second colour table to keep
 * in step.
 *
 * WHY THEY EXIST AT ALL
 *
 * The eight groups are a checklist an operator ticks OFF when the target already holds that
 * data, under time pressure, on a screen that also carries branch maps and file uploads.
 * Shape is what makes a row findable at a glance; the label and the detail line stay exactly
 * as they were, so nothing here is the only carrier of meaning.
 *
 * ACCESSIBILITY
 *
 * aria-hidden and focusable="false" on every one. The label beside each icon already names
 * the group, so announcing the glyph too would read it twice — and a decorative graphic in
 * the accessibility tree is worse than no graphic. Nothing here conveys state; state is text.
 */

/** The paths, keyed by the group they belong to. One entry per IMPORT_GROUPS member. */
const PATHS: Record<ImportGroup, React.ReactNode> = {
  // people — two figures
  people: (
    <>
      <circle cx="6" cy="5.5" r="2.5" />
      <path d="M1.5 14.5c0-2.5 2-4 4.5-4s4.5 1.5 4.5 4" />
      <circle cx="12" cy="6.5" r="2" />
      <path d="M11.2 10.8c1.9.2 3.3 1.6 3.3 3.7" />
    </>
  ),
  // rooms — a door in a wall
  rooms: (
    <>
      <path d="M2.5 14.5V2.5a1 1 0 0 1 1-1h9a1 1 0 0 1 1 1v12" />
      <path d="M6 14.5v-8h4v8" />
      <circle cx="9" cy="10.5" r=".6" fill="currentColor" stroke="none" />
    </>
  ),
  // patients — a person with a record card
  patients: (
    <>
      <circle cx="8" cy="4.5" r="2.5" />
      <path d="M3 14.5c0-2.8 2.2-4.5 5-4.5s5 1.7 5 4.5" />
      <path d="M8 11.5v2.5" />
    </>
  ),
  // appointments — a calendar
  appointments: (
    <>
      <rect x="2" y="3.5" width="12" height="11" rx="1.5" />
      <path d="M2 6.8h12M5.5 1.8v2.4M10.5 1.8v2.4" />
      <path d="M5.5 9.8h2v2h-2z" fill="currentColor" stroke="none" />
    </>
  ),
  // catalog — a tag with a hole, i.e. a priced thing
  catalog: (
    <>
      <path d="M8.6 1.9H13a1.1 1.1 0 0 1 1.1 1.1v4.4a1 1 0 0 1-.3.7l-6 6a1 1 0 0 1-1.4 0L2.1 9.9a1 1 0 0 1 0-1.4l6-6a1 1 0 0 1 .5-.6z" />
      <circle cx="11" cy="5" r="1.1" />
    </>
  ),
  // stock — stacked lots
  stock: (
    <>
      <path d="M2.5 5.5 8 2.8l5.5 2.7L8 8.2z" />
      <path d="M2.5 8.5 8 11.2l5.5-2.7" />
      <path d="M2.5 11.5 8 14.2l5.5-2.7" />
    </>
  ),
  // money — a receipt with a torn edge and lines of charges
  money: (
    <>
      <path d="M4 1.8h8v12.4l-1.3-1-1.4 1-1.3-1-1.4 1-1.3-1-1.3 1z" />
      <path d="M6.2 5h3.6M6.2 7.6h3.6M6.2 10.2h2.2" />
    </>
  ),
  // loyalty — a star, i.e. points saved
  loyalty: (
    <path d="M8 2.2l1.9 3.9 4.3.6-3.1 3 .7 4.2L8 11.9l-3.8 2 .7-4.2-3.1-3 4.3-.6z" />
  ),
};

export function GroupIcon({ group }: { group: ImportGroup }) {
  return (
    <svg
      viewBox="0 0 16 16"
      width="16"
      height="16"
      aria-hidden="true"
      focusable="false"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      strokeLinecap="round"
      strokeLinejoin="round"
      // Baseline-ish alignment with the label text next to it, and never shrink: a
      // squashed 14px icon reads as a smudge.
      style={{ flex: "0 0 auto", verticalAlign: "-3px" }}
    >
      {PATHS[group]}
    </svg>
  );
}
