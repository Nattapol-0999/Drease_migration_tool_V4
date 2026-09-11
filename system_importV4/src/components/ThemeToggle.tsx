"use client";

import { useEffect } from "react";

import "./theme-toggle.css";

/**
 * Light/dark switch.
 *
 * The current theme lives on <html data-theme>, put there by the inline script in
 * layout.tsx before first paint. This component reads and writes that attribute rather
 * than holding the theme in React state: state would render the wrong value on the server
 * and correct it on hydration, which is exactly the flash the inline script exists to
 * prevent. Which label is visible is decided in CSS for the same reason.
 *
 * Until the operator clicks, there is NOTHING in localStorage and the machine's setting is
 * followed live — so a laptop that switches to light in the evening switches this too. The
 * first click is what turns a preference into a stored decision, and from then on the
 * machine is ignored. That is the behaviour a toggle implies: it would be strange to press
 * "สว่าง" and have the OS quietly take it back.
 */

export const THEME_KEY = "importv4-theme";

type Theme = "dark" | "light";

function current(): Theme {
  return document.documentElement.dataset.theme === "light" ? "light" : "dark";
}

function apply(theme: Theme) {
  document.documentElement.dataset.theme = theme;
}

export default function ThemeToggle({ corner = false }: { corner?: boolean }) {
  useEffect(() => {
    const media = window.matchMedia("(prefers-color-scheme: light)");
    const followMachine = (e: MediaQueryListEvent) => {
      let stored: string | null = null;
      try {
        stored = window.localStorage.getItem(THEME_KEY);
      } catch {
        // Private mode or blocked storage. Nothing is stored, so the machine still leads,
        // which is the same outcome as an operator who has not chosen yet.
      }
      if (stored === "dark" || stored === "light") return; // their choice stands
      apply(e.matches ? "light" : "dark");
    };
    media.addEventListener("change", followMachine);
    return () => media.removeEventListener("change", followMachine);
  }, []);

  function toggle() {
    const next: Theme = current() === "light" ? "dark" : "light";
    apply(next);
    try {
      window.localStorage.setItem(THEME_KEY, next);
    } catch (err) {
      // Storage can be unavailable (private mode, a locked-down browser profile). The
      // theme still changes for this page; it just will not survive a reload. Logged
      // rather than swallowed so it is findable if somebody reports exactly that.
      console.warn("theme: could not persist the choice, it will reset on reload", err);
    }
  }

  return (
    <button
      type="button"
      className={corner ? "theme-toggle corner" : "theme-toggle"}
      onClick={toggle}
      // The visible label already says what pressing this does, but it is only meaningful
      // next to the icon, so the control names itself for a screen reader too.
      aria-label="สลับธีมสว่าง/มืด"
      title="สลับธีมสว่าง/มืด"
    >
      <span className="on-dark" aria-hidden="true">
        ☀
      </span>
      <span className="on-light" aria-hidden="true">
        ☾
      </span>
      <span className="on-dark">สว่าง</span>
      <span className="on-light">มืด</span>
    </button>
  );
}
