"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import type {
  ClinicWipeRunResponse,
  ClinicWipeSurvey,
  ClinicWipeSurveyResponse,
  TargetInfo,
  TargetsResponse,
} from "@/lib/types";

import "./wipe-clinic.css";

/**
 * Deleting a clinic from a target database, completely.
 *
 * TWO STEPS, NOT ONE (owner's decision ②, 5 Oct 2026)
 *
 * A clinic code is seven characters of hex and says nothing about what is behind it.
 * Measured on the seven clinics removed by hand between 1 and 5 Oct 2026, that ranged from
 * 8 rows — a shell whose import never ran — to 55,042 for a tenant whose patients and
 * catalogue were already in. A single screen that takes a code and deletes cannot tell
 * those apart, and a mistyped character would land on a different live clinic.
 *
 * So the survey comes first and the delete button does not exist until it has answered.
 * What the operator confirms is a clinic NAME and a row count, not the string they typed.
 *
 * THE ENVIRONMENT IS NEVER PRE-SELECTED
 *
 * The select starts on "— เลือก —" and the server refuses a request without a configured
 * target. server-target.ts says why, about imports: "guessing a default here is how a
 * request meant for test lands on production". It matters more for a delete.
 */

type Step = "ask" | "confirm" | "done";

export default function WipeClinicButton() {
  const [open, setOpen] = useState(false);
  const [targets, setTargets] = useState<TargetInfo[]>([]);
  const [targetId, setTargetId] = useState("");
  const [code, setCode] = useState("");
  const [confirmCode, setConfirmCode] = useState("");
  const [step, setStep] = useState<Step>("ask");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [survey, setSurvey] = useState<ClinicWipeSurvey | null>(null);
  const [where, setWhere] = useState("");
  const [done, setDone] = useState<Extract<ClinicWipeRunResponse, { ok: true }> | null>(null);
  const closeRef = useRef<HTMLButtonElement>(null);

  const chosen = targets.find((t) => t.id === targetId) ?? null;

  const reset = useCallback(() => {
    setStep("ask");
    setCode("");
    setConfirmCode("");
    setSurvey(null);
    setDone(null);
    setError("");
    setBusy(false);
  }, []);

  useEffect(() => {
    if (!open) return;
    closeRef.current?.focus();
    void (async () => {
      try {
        const res = await fetch("/api/target", { cache: "no-store" });
        const data = (await res.json()) as TargetsResponse;
        setTargets(data.targets ?? []);
      } catch {
        setError("อ่านรายการปลายทางไม่ได้");
      }
    })();
  }, [open]);

  // Escape closes, as it does for every other dialog in this tool — except while the
  // delete is in flight, when closing would hide the only report of what happened.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !busy) setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, busy]);

  // Changing either input invalidates a survey that was already answered: it described a
  // different clinic, or one on a different server.
  useEffect(() => {
    setStep("ask");
    setSurvey(null);
    setConfirmCode("");
  }, [targetId, code]);

  async function runSurvey() {
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/clinic-wipe", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ action: "survey", targetId, clinicCode: code.trim() }),
      });
      const data = (await res.json()) as ClinicWipeSurveyResponse;
      if (!res.ok || !data.ok) {
        setError(("error" in data && data.error) || `สำรวจไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }
      setSurvey(data.survey);
      setWhere(data.target);
      if (!data.survey.found) {
        setError(`ไม่พบคลินิก ${code.trim()} ที่ ${data.target} — ไม่มีอะไรให้ลบ`);
        return;
      }
      setStep("confirm");
    } catch (err) {
      setError(err instanceof Error ? err.message : "สำรวจไม่สำเร็จ");
    } finally {
      setBusy(false);
    }
  }

  async function runWipe() {
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/clinic-wipe", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          action: "wipe",
          targetId,
          clinicCode: code.trim(),
          confirm: confirmCode.trim(),
        }),
      });
      const data = (await res.json()) as ClinicWipeRunResponse;
      if (!res.ok || !data.ok) {
        const stage =
          "stage" in data && data.stage === "rehearsal"
            ? "\n\nซ้อมแห้งไม่ผ่าน จึงไม่มีอะไรถูกลบ"
            : "";
        setError((("error" in data && data.error) || `ลบไม่สำเร็จ (HTTP ${res.status})`) + stage);
        return;
      }
      setDone(data);
      setStep("done");
    } catch (err) {
      setError(err instanceof Error ? err.message : "ลบไม่สำเร็จ");
    } finally {
      setBusy(false);
    }
  }

  const canSurvey = targetId !== "" && code.trim() !== "" && !busy;
  const canWipe = step === "confirm" && confirmCode.trim() === code.trim() && !busy;

  return (
    <>
      <button
        type="button"
        className="wipe-clinic-btn"
        onClick={() => {
          reset();
          setOpen(true);
        }}
        title="ลบคลินิกออกจากปลายทางทั้งหมด"
      >
        ลบคลินิก
      </button>

      {open && (
        <div
          className="wc-backdrop"
          role="dialog"
          aria-modal="true"
          aria-label="ลบคลินิกออกจากปลายทาง"
          onClick={(e) => {
            if (e.target === e.currentTarget && !busy) setOpen(false);
          }}
        >
          <div className="wc-panel">
            <div className="wc-head">
              <h2>ลบคลินิกออกจากปลายทาง</h2>
              <button
                ref={closeRef}
                type="button"
                className="wc-close"
                onClick={() => !busy && setOpen(false)}
                disabled={busy}
                aria-label="ปิด"
              >
                ✕
              </button>
            </div>

            <p className="wc-note">
              ลบทุกแถวของคลินิกนั้นออกจากฐานข้อมูลปลายทาง ทุกตารางที่มี{" "}
              <code>clinic_code</code> รวมถึงตารางลูกที่ไม่มีคอลัมน์นั้นแต่ผูกอยู่ด้วย
              foreign key · <strong>ไม่สำรองข้อมูลให้</strong> (ใช้ตอนจะนำเข้าใหม่ทับ) ·{" "}
              <code>biz_event</code> ไม่ถูกลบ เพราะเป็นบันทึกตรวจสอบที่เขียนแล้วแก้ไม่ได้
            </p>

            <label className="wc-field">
              <span>ปลายทาง</span>
              <select
                value={targetId}
                onChange={(e) => setTargetId(e.target.value)}
                disabled={busy || step === "done"}
              >
                <option value="">— เลือก —</option>
                {targets.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.label}
                    {t.isProduction ? " (ฐานจริง)" : ""} — {t.description}
                  </option>
                ))}
              </select>
            </label>

            {chosen?.isProduction && step !== "done" && (
              <div className="wc-prod">
                <strong>นี่คือฐานข้อมูลจริง</strong> — {chosen.description}
                <br />
                แถวที่ลบไปไม่มีการสำรอง และกู้คืนจากที่นี่ไม่ได้
              </div>
            )}

            <label className="wc-field">
              <span>clinic_code</span>
              <input
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="เช่น C9A4FBB"
                spellCheck={false}
                autoComplete="off"
                disabled={busy || step === "done"}
              />
            </label>

            {error && <div className="wc-err">{error}</div>}

            {step === "confirm" && survey?.found && (
              <>
                <div className="wc-sum">
                  <dl>
                    <dt>คลินิก</dt>
                    <dd>
                      {survey.engName || "(ไม่มีชื่ออังกฤษ)"}
                      {survey.thaName && survey.thaName !== survey.engName
                        ? ` · ${survey.thaName}`
                        : ""}
                    </dd>
                    <dt>รหัส</dt>
                    <dd>
                      <code>{survey.clinicCode}</code>
                    </dd>
                    <dt>สร้างเมื่อ</dt>
                    <dd>{survey.createdAt || "—"}</dd>
                    <dt>สาขา</dt>
                    <dd>
                      {survey.branches.length === 0
                        ? "—"
                        : survey.branches
                            .map((b) => `${b.branchCode || b.code}${b.thaName ? ` (${b.thaName})` : ""}`)
                            .join(" · ")}
                    </dd>
                    <dt>แถวที่จะลบ</dt>
                    <dd>
                      <strong>{survey.totalRows.toLocaleString()}</strong> แถว จาก{" "}
                      {survey.tables.length + survey.orphans.length} ตาราง (สแกน{" "}
                      {survey.baseTablesScanned} ตาราง)
                    </dd>
                    <dt>ใบเสร็จ</dt>
                    <dd>
                      {survey.receipts.toLocaleString()} ใบ · เงินในใบเสร็จ{" "}
                      {survey.receiptMoney} บาท
                    </dd>
                    <dt>ล็อกอินที่จะหลุด</dt>
                    <dd>{survey.liveSessions.toLocaleString()}</dd>
                    <dt>
                      <code>biz_event</code>
                    </dt>
                    <dd>{survey.bizEventRows.toLocaleString()} แถว — เก็บไว้ ไม่ลบ</dd>
                  </dl>
                </div>

                {(survey.tables.length > 0 || survey.orphans.length > 0) && (
                  <div className="wc-tables">
                    <table>
                      <thead>
                        <tr>
                          <th>ตาราง</th>
                          <th className="num">แถว</th>
                        </tr>
                      </thead>
                      <tbody>
                        {survey.tables.map((t) => (
                          <tr key={t.table}>
                            <td>{t.table}</td>
                            <td className="num">{t.rows.toLocaleString()}</td>
                          </tr>
                        ))}
                        {survey.orphans.map((t) => (
                          <tr key={`orphan:${t.table}:${t.via}`}>
                            <td>
                              {t.table}
                              <br />
                              <span className="wc-via">{t.via}</span>
                            </td>
                            <td className="num">{t.rows.toLocaleString()}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}

                <label className="wc-field">
                  <span>พิมพ์ clinic_code ซ้ำเพื่อยืนยัน</span>
                  <input
                    value={confirmCode}
                    onChange={(e) => setConfirmCode(e.target.value)}
                    placeholder={survey.clinicCode}
                    spellCheck={false}
                    autoComplete="off"
                    disabled={busy}
                  />
                </label>
              </>
            )}

            {step === "done" && done && (
              <>
                <div className="wc-ok">
                  <strong>ลบแล้ว</strong> — {done.before.engName || done.before.clinicCode} ที่{" "}
                  {done.target}
                  <br />
                  ลบไป <strong>{done.deletedRows.toLocaleString()}</strong> แถว ·{" "}
                  ตรวจซ้ำจากคอนเนกชันใหม่แล้วเหลือ{" "}
                  <strong>{done.after.totalRows.toLocaleString()}</strong> แถว ·{" "}
                  <code>biz_event</code> เก็บไว้ {done.bizEventKept.toLocaleString()} แถว ·{" "}
                  {done.seconds} วินาที
                  {done.after.found && (
                    <>
                      <br />
                      ⚠ ยังมีแถว <code>clinic</code> เหลืออยู่ — ตรวจด้วยมือ
                    </>
                  )}
                </div>
                <pre className="wc-log">
                  {`ซ้อมแห้ง (ROLLBACK)\n${done.rehearsal || "(ไม่มีข้อความ)"}\n\nรอบจริง (COMMIT)\n${done.applied || "(ไม่มีข้อความ)"}`}
                </pre>
              </>
            )}

            <div className="wc-actions">
              {busy && <span className="wc-spacer">กำลังทำงาน…</span>}
              {step === "done" ? (
                <>
                  <button type="button" className="wc-go" onClick={reset}>
                    ลบคลินิกอื่น
                  </button>
                  <button type="button" className="wc-ghost" onClick={() => setOpen(false)}>
                    ปิด
                  </button>
                </>
              ) : (
                <>
                  <button
                    type="button"
                    className="wc-ghost"
                    onClick={() => setOpen(false)}
                    disabled={busy}
                  >
                    ยกเลิก
                  </button>
                  {step === "ask" ? (
                    <button
                      type="button"
                      className="wc-go"
                      onClick={runSurvey}
                      disabled={!canSurvey}
                    >
                      ตรวจ
                    </button>
                  ) : (
                    <button
                      type="button"
                      className="wc-danger"
                      onClick={runWipe}
                      disabled={!canWipe}
                    >
                      ลบ {survey?.totalRows.toLocaleString()} แถว
                    </button>
                  )}
                </>
              )}
            </div>
          </div>
        </div>
      )}
    </>
  );
}
