"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import "./clear-staging.css";

/**
 * Clearing what unfinished imports left on this box.
 *
 * The list is always shown before anything is deleted, and only ticked rows are sent —
 * there is no "clear everything" button, because the thing being deleted is a 300 MB to
 * 8 GB dump someone may still be working with. Rows the server marked busy cannot be
 * ticked, and the server checks again before it unlinks.
 */

interface StagedFile {
  name: string;
  bytes: number;
}

interface SessionEntry {
  id: string;
  files: StagedFile[];
  bytes: number;
  modifiedAt: number;
  busy: boolean;
  busyReason?: string;
}

interface JobEntry {
  id: string;
  kind?: string;
  state?: string;
  clinicCode?: string;
  files: StagedFile[];
  bytes: number;
  modifiedAt: number;
  orphaned: boolean;
  busy: boolean;
  busyReason?: string;
}

interface Inventory {
  sessions: SessionEntry[];
  jobs: JobEntry[];
  sessionBytes: number;
  jobBytes: number;
  totalBytes: number;
  runningJobs: number;
  root: string;
}

interface ClearResult {
  removedSessions: number;
  removedJobs: number;
  bytes: number;
  refused: { id: string; kind: "session" | "job"; why: string }[];
}

function fmtBytes(n: number): string {
  if (n <= 0) return "0";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

function fmtAge(ms: number): string {
  if (!ms) return "—";
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 90) return `${s} วินาทีที่แล้ว`;
  const m = Math.round(s / 60);
  if (m < 90) return `${m} นาทีที่แล้ว`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h} ชั่วโมงที่แล้ว`;
  return `${Math.round(h / 24)} วันที่แล้ว`;
}

const KIND_LABEL: Record<string, string> = {
  pull: "ดึงข้อมูล",
  generate: "สร้างสคริปต์",
  import: "นำเข้า",
  survey: "สำรวจสาขา",
};

export default function ClearStagingButton() {
  const [open, setOpen] = useState(false);
  const [inv, setInv] = useState<Inventory | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [confirming, setConfirming] = useState(false);
  const [result, setResult] = useState<ClearResult | null>(null);
  const closeRef = useRef<HTMLButtonElement>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const res = await fetch("/api/staging", { cache: "no-store" });
      const data = (await res.json()) as { ok: boolean; error?: string; inventory?: Inventory };
      if (!res.ok || !data.ok || !data.inventory) {
        setError(data.error ?? `อ่านรายการไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }
      setInv(data.inventory);
      setPicked(new Set());
      setConfirming(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "อ่านรายการไม่สำเร็จ");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!open) return;
    void load();
    closeRef.current?.focus();
  }, [open, load]);

  // Escape closes, as it does for every other dialog in this tool.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  const key = (kind: "session" | "job", id: string) => `${kind}:${id}`;

  function toggle(kind: "session" | "job", id: string) {
    setConfirming(false);
    setPicked((prev) => {
      const next = new Set(prev);
      const k = key(kind, id);
      if (next.has(k)) next.delete(k);
      else next.add(k);
      return next;
    });
  }

  const removable = inv
    ? [
        ...inv.sessions.filter((s) => !s.busy).map((s) => key("session", s.id)),
        ...inv.jobs.filter((j) => !j.busy).map((j) => key("job", j.id)),
      ]
    : [];

  const pickedBytes = inv
    ? [...inv.sessions, ...inv.jobs].reduce((n, e) => {
        const k = "orphaned" in e ? key("job", e.id) : key("session", e.id);
        return picked.has(k) ? n + e.bytes : n;
      }, 0)
    : 0;

  function selectAllRemovable() {
    setConfirming(false);
    setPicked(new Set(removable));
  }

  async function clearPicked() {
    if (!confirming) {
      setConfirming(true);
      return;
    }
    setLoading(true);
    setError("");
    try {
      const sessions: string[] = [];
      const jobs: string[] = [];
      for (const k of picked) {
        const [kind, id] = k.split(":");
        if (kind === "session") sessions.push(id);
        else jobs.push(id);
      }
      const res = await fetch("/api/staging", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ sessions, jobs }),
      });
      const data = (await res.json()) as {
        ok: boolean;
        error?: string;
        result?: ClearResult;
        inventory?: Inventory;
      };
      if (!res.ok || !data.ok || !data.result) {
        setError(data.error ?? `ล้างไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }
      setResult(data.result);
      if (data.inventory) setInv(data.inventory);
      setPicked(new Set());
      setConfirming(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "ล้างไม่สำเร็จ");
    } finally {
      setLoading(false);
    }
  }

  return (
    <>
      <button type="button" className="clear-staging-btn" onClick={() => setOpen(true)}>
        ล้างข้อมูลค้าง
      </button>

      {open ? (
        <div
          className="cs-backdrop"
          role="presentation"
          onClick={(e) => {
            if (e.target === e.currentTarget) setOpen(false);
          }}
        >
          <div className="cs-panel" role="dialog" aria-modal="true" aria-labelledby="cs-title">
            <div className="cs-head">
              <h2 id="cs-title">ข้อมูลที่ค้างอยู่บนเครื่องนี้</h2>
              <button
                type="button"
                className="cs-close"
                onClick={() => setOpen(false)}
                ref={closeRef}
                aria-label="ปิด"
              >
                ✕
              </button>
            </div>

            <p className="cs-sub">
              ไฟล์ที่ดึงมาจากระบบเก่า (MySQL · MongoDB · รูป · โลโก้ · รูปโปรไฟล์) และสคริปต์ที่สร้างไว้
              แต่ขั้นตอนไม่จบ จึงยังอยู่บนเครื่องนี้ · ลบที่นี่ไม่กระทบฐานข้อมูลคลินิกและไม่กระทบ V4
              {inv ? (
                <>
                  {" · "}
                  <code>{inv.root}</code>
                </>
              ) : null}
            </p>

            {error ? (
              <p className="cs-error" role="alert">
                {error}
              </p>
            ) : null}

            {inv && inv.runningJobs > 0 ? (
              <p className="cs-warn">
                มีงานกำลังทำอยู่ {inv.runningJobs} งาน — ไฟล์ที่ดึงมาจะยังลบไม่ได้
                เพราะงานนั้นอาจกำลังอ่านอยู่ (สคริปต์ของงานที่จบแล้วยังลบได้)
              </p>
            ) : null}

            {result ? (
              <div className="cs-result" role="status">
                ล้างแล้ว: ไฟล์ที่ดึงมา {result.removedSessions} ชุด · สคริปต์ {result.removedJobs} งาน
                · คืนพื้นที่ {fmtBytes(result.bytes)}
                {result.refused.length > 0 ? (
                  <ul className="cs-refused">
                    {result.refused.map((r) => (
                      <li key={`${r.kind}:${r.id}`}>
                        ไม่ลบ {r.kind === "session" ? "ไฟล์ที่ดึงมา" : "สคริปต์"}{" "}
                        <code>{r.id.slice(0, 8)}</code> — {r.why}
                      </li>
                    ))}
                  </ul>
                ) : null}
              </div>
            ) : null}

            {loading && !inv ? <p className="cs-muted">กำลังอ่าน…</p> : null}

            {inv ? (
              <>
                <div className="cs-group">
                  <h3>
                    ไฟล์ที่ดึงมา ({inv.sessions.length} ชุด · {fmtBytes(inv.sessionBytes)})
                  </h3>
                  {inv.sessions.length === 0 ? (
                    <p className="cs-muted">ไม่มี</p>
                  ) : (
                    <table className="cs-table">
                      <thead>
                        <tr>
                          <th />
                          <th>รหัส</th>
                          <th>ไฟล์</th>
                          <th className="cs-num">ขนาด</th>
                          <th>แก้ไขล่าสุด</th>
                        </tr>
                      </thead>
                      <tbody>
                        {inv.sessions.map((s) => (
                          <tr key={s.id} className={s.busy ? "cs-busy" : undefined}>
                            <td>
                              <input
                                type="checkbox"
                                checked={picked.has(key("session", s.id))}
                                disabled={s.busy || loading}
                                onChange={() => toggle("session", s.id)}
                                aria-label={`เลือกไฟล์ที่ดึงมา ${s.id}`}
                              />
                            </td>
                            <td>
                              <code>{s.id.slice(0, 8)}</code>
                              {s.busy ? <div className="cs-why">{s.busyReason}</div> : null}
                            </td>
                            <td className="cs-files">
                              {s.files.length === 0
                                ? "(ว่าง)"
                                : s.files
                                    .map((f) => `${f.name} ${fmtBytes(f.bytes)}`)
                                    .join(" · ")}
                            </td>
                            <td className="cs-num">{fmtBytes(s.bytes)}</td>
                            <td>{fmtAge(s.modifiedAt)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </div>

                <div className="cs-group">
                  <h3>
                    สคริปต์ที่สร้างไว้ ({inv.jobs.length} งาน · {fmtBytes(inv.jobBytes)})
                  </h3>
                  {inv.jobs.length === 0 ? (
                    <p className="cs-muted">ไม่มี</p>
                  ) : (
                    <table className="cs-table">
                      <thead>
                        <tr>
                          <th />
                          <th>รหัส</th>
                          <th>งาน</th>
                          <th>ไฟล์</th>
                          <th className="cs-num">ขนาด</th>
                          <th>แก้ไขล่าสุด</th>
                        </tr>
                      </thead>
                      <tbody>
                        {inv.jobs.map((j) => (
                          <tr key={j.id} className={j.busy ? "cs-busy" : undefined}>
                            <td>
                              <input
                                type="checkbox"
                                checked={picked.has(key("job", j.id))}
                                disabled={j.busy || loading}
                                onChange={() => toggle("job", j.id)}
                                aria-label={`เลือกสคริปต์ ${j.id}`}
                              />
                            </td>
                            <td>
                              <code>{j.id.slice(0, 8)}</code>
                              {j.busy ? <div className="cs-why">{j.busyReason}</div> : null}
                            </td>
                            <td>
                              {j.kind ? (KIND_LABEL[j.kind] ?? j.kind) : "—"}
                              {j.clinicCode ? ` · ${j.clinicCode}` : ""}
                              {j.orphaned ? (
                                <div className="cs-why">เหลือจากการรีสตาร์ตเซิร์ฟเวอร์</div>
                              ) : j.state === "failed" ? (
                                <div className="cs-why">ล้มเหลว</div>
                              ) : null}
                            </td>
                            <td className="cs-files">
                              {j.files.length === 0
                                ? "(ว่าง)"
                                : j.files
                                    .map((f) => `${f.name} ${fmtBytes(f.bytes)}`)
                                    .join(" · ")}
                            </td>
                            <td className="cs-num">{fmtBytes(j.bytes)}</td>
                            <td>{fmtAge(j.modifiedAt)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  )}
                </div>

                <div className="cs-foot">
                  <button
                    type="button"
                    className="cs-secondary"
                    onClick={selectAllRemovable}
                    disabled={loading || removable.length === 0}
                  >
                    เลือกทั้งหมดที่ลบได้ ({removable.length})
                  </button>
                  <button
                    type="button"
                    className="cs-secondary"
                    onClick={() => void load()}
                    disabled={loading}
                  >
                    โหลดรายการใหม่
                  </button>
                  <span className="cs-spacer" />
                  <button
                    type="button"
                    className="cs-danger"
                    onClick={() => void clearPicked()}
                    disabled={loading || picked.size === 0}
                  >
                    {confirming
                      ? `ยืนยันล้าง ${picked.size} รายการ (${fmtBytes(pickedBytes)})`
                      : `ล้างที่เลือก ${picked.size} รายการ (${fmtBytes(pickedBytes)})`}
                  </button>
                </div>
              </>
            ) : null}
          </div>
        </div>
      ) : null}
    </>
  );
}
