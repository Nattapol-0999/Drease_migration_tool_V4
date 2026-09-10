"use client";

import { useEffect, useState } from "react";

import "./busy-overlay.css";

/**
 * BusyOverlay reports a long request in the middle of the screen.
 *
 * `percent` is deliberately nullable. Only the upload has a real number behind it
 * (XMLHttpRequest reports bytes sent); once the bytes are on the server the CLI runs with
 * no progress channel, so the overlay switches to a moving bar and a clock that counts
 * real elapsed seconds. Filling that stretch with a made-up percentage would tell the
 * operator something the program does not know.
 */
export interface BusyState {
  /** Heading — what the operator asked for. */
  title: string;
  /** Which stage it is in right now, in plain words. */
  phase: string;
  /** 0–100 when it is measured; null while the server works. */
  percent: number | null;
  /** Bytes line under the bar, e.g. "84.2 / 241.7 MB". Omitted when not uploading. */
  detail?: string;
  /** Epoch ms the request started, used for the clock. */
  startedAt: number;
  /** Standing warning, e.g. do not close the tab. */
  note?: string;
}

export default function BusyOverlay({ state }: { state: BusyState | null }) {
  // Re-render once a second so the clock moves. Only mounted while a request is running.
  const [, setTick] = useState(0);
  const running = state !== null;

  useEffect(() => {
    if (!running) return;
    const id = setInterval(() => setTick((n) => n + 1), 1000);
    return () => clearInterval(id);
  }, [running]);

  if (!state) return null;

  const seconds = Math.max(0, Math.round((Date.now() - state.startedAt) / 1000));
  const clock =
    seconds < 60
      ? `${seconds} วินาที`
      : `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")} นาที`;
  const measured = state.percent !== null;
  const pct = measured ? Math.max(0, Math.min(100, state.percent as number)) : 0;

  return (
    <div
      className="busy-overlay"
      role="dialog"
      aria-modal="true"
      aria-label={state.title}
      aria-busy="true"
    >
      <div className="busy-card">
        <h2>{state.title}</h2>
        <div className="busy-phase">{state.phase}</div>

        {measured && (
          <div className="busy-pct" aria-hidden="true">
            {pct.toFixed(0)}%
          </div>
        )}

        <div
          className={measured ? "busy-bar" : "busy-bar indeterminate"}
          role="progressbar"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={measured ? Math.round(pct) : undefined}
          aria-valuetext={measured ? `${pct.toFixed(0)} เปอร์เซ็นต์` : "ยังไม่ทราบความคืบหน้า"}
        >
          <i style={measured ? { width: `${pct}%` } : undefined} />
        </div>

        <div className="busy-meta">
          <span>{state.detail ?? ""}</span>
          <span>ผ่านไป {clock}</span>
        </div>

        {/* aria-live so a screen reader hears the stage change without stealing focus. */}
        <div aria-live="polite" className="sr-only" style={{ position: "absolute", left: -9999 }}>
          {state.phase}
        </div>

        {state.note && <div className="busy-note">{state.note}</div>}
      </div>
    </div>
  );
}
