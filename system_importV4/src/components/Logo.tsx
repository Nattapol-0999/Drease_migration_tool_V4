import "./logo.css";

/**
 * Logo is the mark plus the wordmark, as one lockup.
 *
 * WHY IT IS A COMPONENT AND NOT MARKUP IN TWO PAGES
 *
 * It appears in the home header and on the login card. Pasted into both, they drift — one
 * gets a size tweak the other does not, and a month later the login page has yesterday's
 * logo. There is one definition and both pages call it.
 *
 * WHY THE SVG IS DRAWN HERE RATHER THAN IMPORTED
 *
 * The glyph is the same one as src/app/icon.svg, deliberately: the tab icon and the header
 * should be the same mark. It is duplicated as source rather than shared because the two
 * are consumed in ways that cannot be reconciled — Next's App Router picks up icon.svg by
 * FILENAME and serves it as a file to the browser chrome, while this one is inlined into the
 * document so it can read the theme. Importing an .svg as a React component would mean
 * adding a loader (SVGR) to the build for a 6-line drawing.
 *
 * The pairing is stated in both files so whoever changes one knows the other exists. If the
 * real Dr.Ease logo arrives, both get replaced together — see the note in icon.svg.
 *
 * WHY THE COLOURS ARE TOKENS HERE AND FIXED THERE
 *
 * This SVG lives inside the document, so var(--accent) resolves and the mark follows the
 * light/dark switch (the light theme darkens accent to #1f5fd0 for contrast, and the mark
 * gets that for free). icon.svg cannot do this: a favicon is rendered by the browser
 * outside the document and never sees the app's tokens, which is why it hard-codes them.
 *
 * ACCESSIBILITY
 *
 * The mark is aria-hidden and the wordmark is real text, so a screen reader announces
 * "System ImportV4" once rather than reading a decorative image and then the same words
 * again. It is also selectable and searchable text, which an image of a name is not.
 */
export function Logo() {
  return (
    <span className="logo">
      <svg
        className="logo-mark"
        viewBox="0 0 64 64"
        aria-hidden="true"
        focusable="false"
      >
        <rect width="64" height="64" rx="14" fill="var(--accent)" />
        {/* the container: the target system, open at the top to receive */}
        <path
          d="M18 34v9a3 3 0 0 0 3 3h22a3 3 0 0 0 3-3v-9"
          fill="none"
          stroke="var(--on-accent)"
          strokeWidth="4.5"
          strokeLinecap="round"
        />
        {/* the import itself: data coming down into it */}
        <path
          d="M32 14v18"
          fill="none"
          stroke="var(--on-accent)"
          strokeWidth="4.5"
          strokeLinecap="round"
        />
        <path
          d="M24 25l8 8 8-8"
          fill="none"
          stroke="var(--on-accent)"
          strokeWidth="4.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
      <span className="logo-text">System ImportV4</span>
    </span>
  );
}

export default Logo;
