"use client";

import { useEffect, useState } from "react";
import {
  CLI_INPUTS,
  IMPORT_GROUPS,
  type CliInputField,
  type GenerateResponse,
  type ImportGroup,
  type LegacySourceStatus,
  type PullResponse,
} from "@/lib/types";

/** base64 → bytes, so a pulled dump becomes a File with its bytes intact. */
function base64ToBytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function human(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

export default function GenerateForm() {
  const [clinic, setClinic] = useState("");
  const [owner, setOwner] = useState("");
  const [files, setFiles] = useState<Partial<Record<CliInputField, File>>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [result, setResult] = useState<{ filename: string; sql: string; report: string } | null>(null);

  // Where the dumps come from: tab 1 = แนบไฟล์เอง · tab 2 = ให้เซิร์ฟเวอร์ไปดึงมาให้.
  // Both fill the same `files` slots, so everything below panel 2 is unchanged.
  const [tab, setTab] = useState<"file" | "server">("file");
  const [legacy, setLegacy] = useState<LegacySourceStatus | null>(null);
  const [host, setHost] = useState("");
  const [pulling, setPulling] = useState(false);
  const [pullErr, setPullErr] = useState<string | null>(null);
  const [pullLog, setPullLog] = useState<string[] | null>(null);
  const [pullWarnings, setPullWarnings] = useState<string[]>([]);
  const [pulledFrom, setPulledFrom] = useState<string | null>(null);
  const [pulledFields, setPulledFields] = useState<CliInputField[]>([]);

  // "import to server" state. The target comes from the server so the browser never
  // sees the key path or the password.
  const [target, setTarget] = useState<{ configured: boolean; description?: string; missing?: string[] } | null>(null);
  const [serverConfirm, setServerConfirm] = useState("");
  const [importing, setImporting] = useState(false);
  const [checking, setChecking] = useState(false);
  // Everything by default; an operator narrows it when the target already has some.
  const [groups, setGroups] = useState<ImportGroup[]>(IMPORT_GROUPS.map((g) => g.group));
  const [importErr, setImportErr] = useState<string | null>(null);
  const [importLog, setImportLog] = useState<string | null>(null);
  const [importDone, setImportDone] = useState<string | null>(null);

  useEffect(() => {
    fetch("/api/target")
      .then((r) => r.json())
      .then(setTarget)
      .catch(() => setTarget({ configured: false }));
    fetch("/api/legacy-source")
      .then((r) => r.json())
      .then(setLegacy)
      .catch(() => setLegacy({ configured: false }));
  }, []);

  const chosen = CLI_INPUTS.filter((i) => files[i.field]);

  async function submit() {
    setErr(null);
    setResult(null);
    if (!clinic.trim()) {
      setErr("กรอก clinic_code ก่อน (ต้องเป็นคลินิกที่สร้างไว้ในระบบแล้ว)");
      return;
    }
    if (chosen.length === 0) {
      setErr("เลือกไฟล์ dump อย่างน้อยหนึ่งไฟล์");
      return;
    }
    if (files.sql && !owner.trim()) {
      setErr("ไฟล์ SQL มีตาราง users → ต้องระบุ owner_username");
      return;
    }

    const body = new FormData();
    body.append("clinic_code", clinic.trim());
    if (owner.trim()) body.append("owner_username", owner.trim());
    // Only send `only` when it is a real subset; empty means everything.
    if (groups.length > 0 && groups.length < IMPORT_GROUPS.length) {
      body.append("only", groups.join(","));
    }
    for (const { field } of CLI_INPUTS) {
      const f = files[field];
      if (f) body.append(field, f);
    }

    setBusy(true);
    try {
      const res = await fetch("/api/generate", { method: "POST", body });
      const json = (await res.json()) as GenerateResponse;
      if (!json.ok) {
        setErr(json.error);
        return;
      }
      setResult({ filename: json.filename, sql: json.sql, report: json.report });
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }

  /**
   * pullData asks the server to fetch the dumps off the legacy box. The result lands in
   * the same `files` slots a manual upload fills, so "สร้างไฟล์ .sql" behaves identically
   * whichever tab was used.
   */
  async function pullData() {
    setPullErr(null);
    setPullLog(null);
    setPullWarnings([]);
    if (!host.trim()) {
      setPullErr("กรอก IP ของเซิร์ฟเวอร์ระบบเก่าก่อน");
      return;
    }
    setPulling(true);
    try {
      const res = await fetch("/api/pull-server", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ host: host.trim() }),
      });
      const json = (await res.json()) as PullResponse;
      if (!json.ok) {
        setPullErr(json.error);
        return;
      }
      const next: Partial<Record<CliInputField, File>> = {};
      for (const f of json.files) {
        next[f.field] = new File([base64ToBytes(f.base64)], f.name, { type: "text/plain" });
      }
      setFiles((prev) => ({ ...prev, ...next }));
      setPulledFields((prev) => Array.from(new Set([...prev, ...json.files.map((f) => f.field)])));
      setPulledFrom(json.host);
      setPullLog(json.log);
      setPullWarnings(json.warnings);
      // A fresh source invalidates the artifact built from the previous one.
      setResult(null);
    } catch (e) {
      setPullErr(e instanceof Error ? e.message : String(e));
    } finally {
      setPulling(false);
    }
  }

  function clearField(field: CliInputField) {
    setFiles((prev) => {
      const next = { ...prev };
      delete next[field];
      return next;
    });
    setPulledFields((prev) => prev.filter((f) => f !== field));
  }

  function download() {
    if (!result) return;
    const blob = new Blob([result.sql], { type: "application/sql;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = result.filename;
    a.click();
    URL.revokeObjectURL(url);
  }

  async function checkTarget() {
    setImportErr(null);
    setImportDone(null);
    setImportLog(null);
    setChecking(true);
    try {
      const res = await fetch("/api/check-target", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ clinicCode: clinic.trim() }),
      });
      const json = (await res.json()) as {
        ok: boolean;
        error?: string;
        log?: string;
        target?: string;
        clinicExists?: boolean;
      };
      setImportLog(json.log ?? null);
      if (!json.ok) {
        setImportErr(json.error ?? "ตรวจไม่ผ่าน");
        return;
      }
      setImportDone(
        json.clinicExists
          ? `เชื่อมต่อได้ · พบ clinic_code ${clinic.trim()} ที่ปลายทาง — พร้อมนำเข้า`
          : `เชื่อมต่อได้ แต่ยังไม่พบ clinic_code ${clinic.trim()} ที่ปลายทาง — ต้องสร้างคลินิกก่อน ไม่งั้นสคริปต์จะหยุดเอง`,
      );
    } catch (e) {
      setImportErr(e instanceof Error ? e.message : String(e));
    } finally {
      setChecking(false);
    }
  }

  async function importToServer() {
    if (!result) return;
    setImportErr(null);
    setImportLog(null);
    setImporting(true);
    try {
      const res = await fetch("/api/import-to-server", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          sql: result.sql,
          clinicCode: clinic.trim(),
          confirm: serverConfirm.trim(),
        }),
      });
      const json = (await res.json()) as {
        ok: boolean;
        error?: string;
        log?: string;
        target?: string;
        seconds?: string;
      };
      setImportLog(json.log ?? null);
      if (!json.ok) {
        setImportErr(json.error ?? "นำเข้าไม่สำเร็จ");
        return;
      }
      setImportDone(`นำเข้าสำเร็จ → ${json.target} (${json.seconds}s)`);
    } catch (e) {
      setImportErr(e instanceof Error ? e.message : String(e));
    } finally {
      setImporting(false);
    }
  }

  return (
    <div>
      <div className="panel">
        <h2>1 · ปลายทาง</h2>
        <div className="msg info">
          คลินิกต้อง <b>สร้างในระบบก่อน</b> — สคริปต์ที่ได้จะตรวจและหยุดทันทีถ้าไม่พบ{" "}
          <code>clinic_code</code> นี้ในฐานข้อมูล
        </div>

        <label>clinic_code (จำเป็น)</label>
        <input
          type="text"
          value={clinic}
          onChange={(e) => setClinic(e.target.value)}
          placeholder="เช่น C9D4865"
        />

        <label>owner_username (จำเป็นเมื่อแนบไฟล์ SQL)</label>
        <input
          type="text"
          value={owner}
          onChange={(e) => setOwner(e.target.value)}
          placeholder="legacy users.code ของบัญชีที่จะเป็น owner"
        />
        <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
          บัญชีนี้ได้ role <code>owner</code> · ที่เหลือได้ <code>counter_staff</code> ·
          บัญชีที่เป็นหมอจะถูกทำเครื่องหมาย <code>doctor</code> ในทะเบียนพนักงานให้อัตโนมัติ
        </div>
      </div>

      <div className="panel">
        <h2>2 · ที่มาของข้อมูล</h2>

        <div className="tabs" role="tablist" aria-label="ที่มาของไฟล์ dump">
          <button
            type="button"
            role="tab"
            id="tab-file"
            aria-selected={tab === "file"}
            aria-controls="panel-file"
            className={`tab${tab === "file" ? " active" : ""}`}
            onClick={() => setTab("file")}
          >
            1 · import ผ่านไฟล์
          </button>
          <button
            type="button"
            role="tab"
            id="tab-server"
            aria-selected={tab === "server"}
            aria-controls="panel-server"
            className={`tab${tab === "server" ? " active" : ""}`}
            onClick={() => setTab("server")}
          >
            2 · import จากเซิร์ฟเวอร์
          </button>
        </div>

        {tab === "file" ? (
          <div role="tabpanel" id="panel-file" aria-labelledby="tab-file">
            <table>
              <thead>
                <tr>
                  <th>ช่อง</th>
                  <th>ไฟล์</th>
                  <th>ปลายทาง</th>
                </tr>
              </thead>
              <tbody>
                {CLI_INPUTS.map((i) => {
                  const loaded = files[i.field];
                  return (
                    <tr key={i.field}>
                      <td>
                        <code>{i.label}</code>
                      </td>
                      <td>
                        <input
                          type="file"
                          onChange={(e) => {
                            const f = e.target.files?.[0];
                            if (!f) {
                              clearField(i.field);
                              return;
                            }
                            setFiles((prev) => ({ ...prev, [i.field]: f }));
                            setPulledFields((prev) => prev.filter((x) => x !== i.field));
                          }}
                        />
                        {loaded && (
                          <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                            {pulledFields.includes(i.field) ? (
                              <span className="tag ok">ดึงจาก {pulledFrom}</span>
                            ) : (
                              <span className="tag">แนบเอง</span>
                            )}{" "}
                            <code>{loaded.name}</code> · {human(loaded.size)}{" "}
                            <button
                              type="button"
                              className="secondary"
                              style={{ padding: "1px 8px", fontSize: 12 }}
                              onClick={() => clearField(i.field)}
                            >
                              ล้าง
                            </button>
                          </div>
                        )}
                      </td>
                      <td className="muted">{i.hint}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            <div className="muted" style={{ fontSize: 12 }}>
              แนบ <b>ไฟล์ dump ทั้งฐาน</b> ได้เลย — ระบบหาตารางที่รองรับให้เอง และรายงานตารางที่มีในไฟล์แต่ยังไม่นำเข้า
            </div>
          </div>
        ) : (
          <div role="tabpanel" id="panel-server" aria-labelledby="tab-server">
            {legacy && !legacy.configured ? (
              <div className="msg warn">
                แท็บนี้ปิดอยู่ — ยังไม่ได้ตั้งค่าเซิร์ฟเวอร์ต้นทางใน <code>.env.local</code>
                {legacy.missing?.length ? <> (ขาด {legacy.missing.join(", ")})</> : null}
              </div>
            ) : (
              <>
                <div className="msg info">
                  ให้เซิร์ฟเวอร์นี้ ssh เข้าเครื่องระบบเก่าแล้ว <b>อ่านข้อมูลออกมา</b> —{" "}
                  <code>mysqldump</code> ตารางที่ใช้ + export แคตตาล็อกจาก MongoDB{" "}
                  <b>ไม่เขียนอะไรลงเครื่องต้นทาง</b>
                  {legacy?.description ? (
                    <>
                      <br />
                      บัญชี/ฐานที่ใช้: <code>{legacy.description}</code>
                    </>
                  ) : null}
                </div>

                <label htmlFor="legacy-host">IP ของเซิร์ฟเวอร์ระบบเก่า</label>
                <input
                  id="legacy-host"
                  type="text"
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  placeholder="เช่น 43.210.146.174"
                  spellCheck={false}
                />
                {legacy?.hostsAllowed?.length ? (
                  <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                    รับเฉพาะที่อยู่ใน <code>LEGACY_HOSTS_ALLOWED</code>:{" "}
                    {legacy.hostsAllowed.join(", ")}
                  </div>
                ) : (
                  <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                    ยังไม่ได้จำกัดปลายทาง — <code>LEGACY_HOSTS_ALLOWED</code> ว่างอยู่ ใส่ IP ให้ถูก
                    เพราะรหัสผ่าน ssh จะถูกส่งไปยังเครื่องที่ตอบรับ
                  </div>
                )}

                <div className="row" style={{ marginTop: 12 }}>
                  <button type="button" onClick={pullData} disabled={pulling || !host.trim()}>
                    {pulling ? "กำลังดึงข้อมูล…" : "pull data"}
                  </button>
                  {pulling && (
                    <span className="muted">
                      กำลังรัน mysqldump บน {host.trim()} — ไฟล์ใหญ่ใช้เวลาสักครู่
                    </span>
                  )}
                </div>

                {pullErr && <div className="msg error">{pullErr}</div>}

                {pulledFrom && !pullErr && (
                  <div className="msg ok">
                    ดึงจาก <code>{pulledFrom}</code> แล้ว — ไฟล์เข้าไปอยู่ในช่องเดียวกับการแนบเอง
                    <ul className="tight">
                      {CLI_INPUTS.filter((i) => pulledFields.includes(i.field)).map((i) => {
                        const f = files[i.field];
                        return (
                          <li key={i.field}>
                            <code>{i.label}</code> — {f ? `${f.name} · ${human(f.size)}` : "—"}
                          </li>
                        );
                      })}
                    </ul>
                  </div>
                )}

                {pullWarnings.map((w) => (
                  <div className="msg warn" key={w}>
                    {w}
                  </div>
                ))}

                {pullLog && pullLog.length > 0 && (
                  <>
                    <h3>ผลจากเครื่องต้นทาง</h3>
                    <pre
                      style={{
                        background: "var(--panel-2)",
                        border: "1px solid var(--border)",
                        borderRadius: 6,
                        padding: 12,
                        overflowX: "auto",
                        fontSize: 12,
                        whiteSpace: "pre-wrap",
                      }}
                    >
                      {pullLog.join("\n")}
                    </pre>
                  </>
                )}
              </>
            )}
          </div>
        )}

        <div className="msg warn" style={{ marginTop: 10 }}>
          ช่อง <code>mongo</code> อ่าน <b>แคตตาล็อกเท่านั้น</b> — collection ฝั่งเงิน
          (<code>bills</code>, <code>payment_summary</code>, <code>course_bills</code>)
          อยู่ในไฟล์เดียวกันแต่ <b>ยังไม่ถูกนำเข้า</b> เพราะยังไม่ได้เคาะวิธีลงบัญชีย้อนหลัง
        </div>
      </div>

      <div className="panel">
        <h2>3 · เลือกสิ่งที่จะนำเข้า</h2>
        <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
          ค่าเริ่มต้นคือทั้งหมด — ติ๊กออกเมื่อปลายทางมีข้อมูลส่วนนั้นอยู่แล้ว
          (การนำเข้าทับจะเขียนค่าจากไฟล์ลงบนของเดิม)
        </div>
        {IMPORT_GROUPS.map((g) => (
          <label key={g.group} style={{ display: "block", margin: "6px 0", color: "var(--text)" }}>
            <input
              type="checkbox"
              checked={groups.includes(g.group)}
              onChange={(e) =>
                setGroups((prev) =>
                  e.target.checked ? [...prev, g.group] : prev.filter((x) => x !== g.group),
                )
              }
              style={{ marginRight: 8 }}
            />
            {g.label} <span className="muted" style={{ fontSize: 12 }}>· {g.detail}</span>
          </label>
        ))}
        {groups.length === 0 && (
          <div className="msg error">เลือกอย่างน้อยหนึ่งกลุ่ม</div>
        )}
      </div>

      <div className="panel">
        <h2>4 · สร้างสคริปต์</h2>
        <div className="row">
          <button onClick={submit} disabled={busy || groups.length === 0}>
            {busy ? "กำลังสร้าง…" : "สร้างไฟล์ .sql"}
          </button>
          {chosen.length > 0 && (
            <span className="muted">เลือกไว้ {chosen.length} ไฟล์</span>
          )}
        </div>

        {err && <div className="msg error">{err}</div>}

        {result && (
          <>
            <div className="msg info">
              สร้างเสร็จ: <code>{result.filename}</code> — <b>อ่านไฟล์ก่อนรัน</b> แล้วรันด้วย
              <br />
              <code>psql &quot;$DATABASE_URL&quot; -v ON_ERROR_STOP=1 -f {result.filename}</code>
            </div>
            <div className="row">
              <button onClick={download}>ดาวน์โหลด {result.filename}</button>
            </div>

            <h3>นำเข้าไปที่เซิร์ฟเวอร์</h3>
            {!target?.configured ? (
              <div className="msg warn">
                ปุ่มนี้ปิดอยู่ — ยังไม่ได้ตั้งค่าปลายทางใน <code>.env.local</code>
                {target?.missing?.length ? <> (ขาด {target.missing.join(", ")})</> : null}
              </div>
            ) : (
              <>
                <div className="msg warn">
                  <b>เขียนลงฐานข้อมูลจริง ย้อนยาก</b> — ปลายทาง:{" "}
                  <code>{target.description}</code>
                  <br />
                  สคริปต์อยู่ในทรานแซกชันเดียว ถ้ามีอะไรผิดจะ rollback ทั้งหมด และถ้าไม่พบ{" "}
                  <code>clinic_code</code> ที่ปลายทาง จะหยุดทันที
                </div>
                <div className="row" style={{ marginBottom: 4 }}>
                  <button className="secondary" onClick={checkTarget} disabled={checking || importing}>
                    {checking ? "กำลังตรวจ…" : "ตรวจการเชื่อมต่อ (อ่านอย่างเดียว)"}
                  </button>
                  <span className="muted" style={{ fontSize: 12 }}>
                    เช็คว่า ssh/psql ต่อได้ และมี clinic_code นี้ที่ปลายทางหรือยัง — ไม่เขียนอะไร
                  </span>
                </div>

                <label>พิมพ์ clinic_code ซ้ำเพื่อยืนยัน</label>
                <input
                  type="text"
                  value={serverConfirm}
                  onChange={(e) => setServerConfirm(e.target.value)}
                  placeholder={clinic.trim() || "clinic_code"}
                />
                <div className="row" style={{ marginTop: 10 }}>
                  <button
                    className="go"
                    onClick={importToServer}
                    disabled={importing || !clinic.trim() || serverConfirm.trim() !== clinic.trim()}
                  >
                    {importing ? "กำลังนำเข้า…" : "import to server"}
                  </button>
                  {importing && <span className="muted">กำลังรัน psql บนเซิร์ฟเวอร์…</span>}
                </div>

                {importDone && <div className="msg ok">{importDone}</div>}
                {importErr && <div className="msg error">{importErr}</div>}
                {importLog && (
                  <>
                    <h3>ผลจาก psql</h3>
                    <pre
                      style={{
                        background: "var(--panel-2)",
                        border: "1px solid var(--border)",
                        borderRadius: 6,
                        padding: 12,
                        overflowX: "auto",
                        fontSize: 12,
                        whiteSpace: "pre-wrap",
                      }}
                    >
                      {importLog}
                    </pre>
                  </>
                )}
              </>
            )}
            <h3>รายงานจาก CLI</h3>
            <pre
              style={{
                background: "var(--panel-2)",
                border: "1px solid var(--border)",
                borderRadius: 6,
                padding: 12,
                overflowX: "auto",
                fontSize: 12,
                whiteSpace: "pre-wrap",
              }}
            >
              {result.report}
            </pre>
          </>
        )}
      </div>
    </div>
  );
}
