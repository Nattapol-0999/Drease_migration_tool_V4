"use client";

import "./busy-overlay.css";

/**
 * DoneOverlay is the counterpart to BusyOverlay: the same card, after the work finished.
 *
 * WHY A MODAL AND NOT JUST THE GREEN LINE
 *
 * The inline result sits at the bottom of a long form. An import runs for minutes, the operator
 * looks away, and when they look back the page appears unchanged — the button is enabled again
 * and the outcome is somewhere below the fold. This puts the answer where the eye already is,
 * because that is where the progress card was a second earlier.
 *
 * IT DOES NOT CLOSE BY ITSELF
 *
 * Owner's instruction, 18 Sep 2026: it stays until dismissed. A toast that fades is exactly what
 * the operator misses while looking away, and this card carries the link they are meant to click.
 * The inline message stays too — this is in addition to it, not instead of it, so the record of
 * what happened survives closing the card.
 */
export interface DoneState {
  /** Heading — what finished. */
  title: string;
  /** The facts, one per line. Already-computed strings; this component invents nothing. */
  lines: string[];
  /**
   * Where to go and look at the result. Present only when the data actually landed somewhere
   * that can be opened — the import steps. A pull or a script build has nothing to show yet,
   * and a link to an app that does not hold the data would be a lie.
   */
  link?: { href: string; label: string };
}

export default function DoneOverlay({
  state,
  onClose,
}: {
  state: DoneState | null;
  onClose: () => void;
}) {
  if (!state) return null;

  return (
    <div className="busy-overlay" role="dialog" aria-modal="true" aria-label={state.title}>
      <div className="busy-card">
        <h2>
          <span aria-hidden="true">✓</span> {state.title}
        </h2>

        <ul className="done-lines">
          {state.lines.map((l) => (
            <li key={l}>{l}</li>
          ))}
        </ul>

        <div className="done-actions">
          {state.link && (
            // rel="noreferrer" as well as noopener: the target is another of our own apps, but
            // the referrer of a migration tool URL is not something to hand to any tab.
            <a
              className="done-link"
              href={state.link.href}
              target="_blank"
              rel="noopener noreferrer"
            >
              {state.link.label} ↗
            </a>
          )}
          <button type="button" className="secondary" onClick={onClose} autoFocus>
            ปิด
          </button>
        </div>
      </div>
    </div>
  );
}
