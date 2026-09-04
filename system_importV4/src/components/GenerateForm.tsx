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
  type ClinicAPIStatus,
  type ClinicAPIStatusResponse,
  type CreateClinicResponse,
  type ProvisionedClinic,
  type TargetId,
  type TargetInfo,
  type TargetsResponse,
} from "@/lib/types";

import "./target-picker.css";

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

  // "import to server" state. The environments come from the server so the browser never
  // sees a host, a key path or a password — only an ID it may send back.
  const [targets, setTargets] = useState<TargetInfo[] | null>(null);
  const [targetProblems, setTargetProblems] = useState<{ id: TargetId; missing: string[] }[]>([]);
  // Always test on load, and deliberately not remembered across a refresh: firing at
  // production should be a choice made now, not one inherited from a previous session.
  const [targetId, setTargetId] = useState<TargetId>("test");
  const [serverConfirm, setServerConfirm] = useState("");

  // Creating the clinic at the target. Availability is probed rather than assumed, so the
  // control is never offered when it cannot work — and says which part is missing.
  const [clinicApi, setClinicApi] = useState<ClinicAPIStatus[] | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createErr, setCreateErr] = useState<string | null>(null);
  const [created, setCreated] = useState<ProvisionedClinic | null>(null);
  const [newThaName, setNewThaName] = useState("");
  const [ownerName, setOwnerName] = useState("");
  const [ownerEmail, setOwnerEmail] = useState("");
  const [importing, setImporting] = useState(false);
  const [checking, setChecking] = useState(false);
  // Everything by default; an operator narrows it when the target already has some.
  const [groups, setGroups] = useState<ImportGroup[]>(IMPORT_GROUPS.map((g) => g.group));
  const [importErr, setImportErr] = useState<string | null>(null);
  const [importLog, setImportLog] = useState<string | null>(null);
  const [importDone, setImportDone] = useState<string | null>(null);

  useEffect(() => {
    fetch("/api/target")
      .then((r) => r.json() as Promise<TargetsResponse>)
      .then((j) => {
        setTargets(j.targets ?? []);
        setTargetProblems(j.problems ?? []);
      })
      .catch(() => setTargets([]));
    fetch("/api/legacy-source")
      .then((r) => r.json())
      .then(setLegacy)
      .catch(() => setLegacy({ configured: false }));
    // Probes each backend; read-only, and the probe deliberately sends an invalid code so
    // it cannot create anything.
    fetch("/api/clinic-api-status")
      .then((r) => r.json() as Promise<ClinicAPIStatusResponse>)
      .then((j) => setClinicApi(j.targets ?? []))
      .catch(() => setClinicApi([]));
  }, []);

  const chosen = CLI_INPUTS.filter((i) => files[i.field]);
  const selected = targets?.find((t) => t.id === targetId) ?? null;
  const onProduction = selected?.isProduction === true;
  const createApi = clinicApi?.find((c) => c.id === targetId) ?? null;

  /**
   * createClinic asks the selected environment's backend to provision the clinic.
   *
   * On success the code is written into the clinic_code field, because that is the value
   * every step below needs and re-typing a generated code is how a typo gets imported.
   */
  async function createClinic() {
    setCreateErr(null);
    setCreated(null);
    setCreating(true);
    try {
      const res = await fetch("/api/create-clinic", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          targetId,
          clinicCode: clinic.trim(),
          thaName: newThaName.trim(),
          ownerName: ownerName.trim(),
          ownerEmail: ownerEmail.trim(),
        }),
      });
      const json = (await res.json()) as CreateClinicResponse;
      if (!json.ok) {
        setCreateErr(json.error);
        return;
      }
      setCreated(json.clinic);
      setClinic(json.clinic.clinic_code);
      // The clinic exists now, so any earlier "not found at the target" verdict is stale.
      setImportDone(null);
      setImportErr(null);
    } catch (e) {
      setCreateErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCreating(false);
    }
  }

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
        body: JSON.stringify({ clinicCode: clinic.trim(), targetId }),
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
      const where = json.target ?? targetId;
      setImportDone(
        json.clinicExists
          ? `เชื่อมต่อได้ (${where}) · พบ clinic_code ${clinic.trim()} ที่ปลายทาง — พร้อมนำเข้า`
          : `เชื่อมต่อได้ (${where}) แต่ยังไม่พบ clinic_code ${clinic.trim()} ที่ปลายทาง — ต้องสร้างคลินิกก่อน ไม่งั้นสคริปต์จะหยุดเอง`,
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
          targetId,
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

        <label htmlFor="target-select">เซิร์ฟเวอร์ปลายทาง</label>
        {targets === null ? (
          <div className="muted" style={{ fontSize: 12 }}>กำลังอ่านรายการปลายทาง…</div>
        ) : targets.length === 0 ? (
          <div className="msg warn">
            ยังไม่มีปลายทางที่ตั้งค่าครบใน <code>.env.local</code>
            {targetProblems.map((p) => (
              <div key={p.id}>
                <code>{p.id}</code> ขาด {p.missing.join(", ")}
              </div>
            ))}
          </div>
        ) : (
          <>
            <div className="tgt-row">
              <select
                id="target-select"
                className={`tgt-select${onProduction ? " is-prod" : ""}`}
                value={targetId}
                onChange={(e) => {
                  setTargetId(e.target.value as TargetId);
                  // A different server makes the previous check meaningless.
                  setImportDone(null);
                  setImportErr(null);
                  setImportLog(null);
                }}
              >
                {targets.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.isProduction ? `${t.label} — ของจริง` : t.label}
                  </option>
                ))}
              </select>
              {selected && <span className="tgt-where">{selected.description}</span>}
            </div>
            {targetProblems.length > 0 && (
              <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                ตั้งค่าไม่ครบ:{" "}
                {targetProblems.map((p) => `${p.id} (ขาด ${p.missing.join(", ")})`).join(" · ")}
              </div>
            )}
          </>
        )}

        {onProduction && (
          <div className="tgt-danger" role="alert">
            <strong>นี่คือเซิร์ฟเวอร์ของจริง</strong> — ทุกอย่างที่กดต่อจากนี้เขียนลงข้อมูลคนไข้จริง
            <br />
            ปลายทาง: <code>{selected?.description}</code>
            <br />
            ระวังเป็นพิเศษ: ชื่อฐานข้อมูลของ <code>production</code> กับ <code>test</code>{" "}
            <b>เหมือนกันทั้งคู่</b> (<code>drease_temporary</code>) สิ่งเดียวที่แยกสองระบบคือ IP
            ด่านพิมพ์ <code>clinic_code</code> ยืนยันจึงไม่ช่วยจับกรณีเลือกเซิร์ฟเวอร์ผิด
            <br />
            ถ้าคลินิกนี้มีข้อมูลอยู่แล้วที่ปลายทาง การนำเข้าสิทธิ์คอร์สจะ{" "}
            <b>ลบแถวเดิมแล้วสร้างใหม่</b> — กด &quot;ตรวจการเชื่อมต่อ&quot; ก่อนเพื่อดูว่ามีอะไรอยู่
          </div>
        )}

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

        {/* Creating the clinic at the target. Shown as its own step because it WRITES to
            the target, unlike everything else in this panel. */}
        {clinicApi === null ? (
          <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
            กำลังตรวจว่าสร้างคลินิกที่ปลายทางได้ไหม…
          </div>
        ) : !createApi?.available ? (
          <div className="msg warn" style={{ marginTop: 8 }}>
            ยังสร้างคลินิกจากหน้านี้ไม่ได้ — {createApi?.reason ?? "ไม่ทราบสาเหตุ"}
            {createApi?.api ? (
              <>
                <br />
                ปลายทางที่จะเรียก: <code>{createApi.api}</code>
              </>
            ) : null}
            <br />
            ระหว่างนี้ต้องสร้างคลินิกในระบบ V4 เองก่อน แล้วกรอก <code>clinic_code</code> ข้างบน
          </div>
        ) : !showCreate ? (
          <div className="row" style={{ marginTop: 8 }}>
            <button type="button" className="secondary" onClick={() => setShowCreate(true)}>
              ยังไม่มีคลินิกนี้? สร้างที่ปลายทาง
            </button>
            <span className="muted" style={{ fontSize: 12 }}>
              เรียก backend ของ {selected?.label ?? targetId} ให้สร้างให้ — เว้น{" "}
              <code>clinic_code</code> ว่างไว้ได้ ระบบจะสุ่มรหัสให้
            </span>
          </div>
        ) : (
          <div className={onProduction ? "tgt-danger" : "panel"} style={{ marginTop: 8 }}>
            <b>สร้างคลินิกที่ {selected?.label ?? targetId}</b>
            {onProduction && (
              <>
                {" "}
                — <strong>เซิร์ฟเวอร์ของจริง</strong>
              </>
            )}
            <div className="muted" style={{ fontSize: 12, margin: "4px 0 8px" }}>
              สร้างตัวคลินิก + สาขาแรก + ฟีเจอร์ + subscription ในทรานแซกชันเดียว ·
              เรียกซ้ำด้วยรหัสเดิมไม่สร้างของซ้ำ (รายงานว่าอะไรมีอยู่แล้ว)
            </div>

            <label htmlFor="new-clinic-name">ชื่อคลินิก (ไม่ใส่ก็ได้ — จะใช้ clinic_code เป็นชื่อ)</label>
            <input
              id="new-clinic-name"
              type="text"
              value={newThaName}
              onChange={(e) => setNewThaName(e.target.value)}
              placeholder="เช่น คลินิกตัวอย่าง"
            />

            <h3 style={{ marginTop: 14 }}>บัญชี owner (ไม่ใส่ก็ได้)</h3>
            <div className="muted" style={{ fontSize: 12, marginBottom: 6 }}>
              ใส่อีเมลแล้วระบบจะสร้างบัญชีผู้ดูแลคลินิกให้ในทรานแซกชันเดียวกัน — ได้ role{" "}
              <code>owner</code> และโผล่ในหน้า Settings › ผู้ใช้งาน ·{" "}
              <b>ไม่ต้องกรอกรหัสผ่าน ระบบสุ่มให้แล้วแสดงครั้งเดียว</b>
            </div>

            <label htmlFor="owner-name">ชื่อ owner</label>
            <input
              id="owner-name"
              type="text"
              value={ownerName}
              onChange={(e) => setOwnerName(e.target.value)}
              placeholder="เช่น คุณสมชาย"
              autoComplete="off"
            />

            <label htmlFor="owner-email">อีเมล owner (ใช้ล็อกอิน)</label>
            <input
              id="owner-email"
              type="email"
              value={ownerEmail}
              onChange={(e) => setOwnerEmail(e.target.value)}
              placeholder="owner@example.com"
              autoComplete="off"
              spellCheck={false}
            />
            <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
              อีเมลต้องไม่ถูกใช้โดยบัญชีของคลินิกอื่น — <code>users.email</code> ไม่ซ้ำได้ทั้งระบบ
            </div>

            <div className="row" style={{ marginTop: 10 }}>
              <button type="button" onClick={createClinic} disabled={creating}>
                {creating
                  ? "กำลังสร้าง…"
                  : clinic.trim()
                    ? `สร้าง ${clinic.trim()}`
                    : "สร้าง (สุ่มรหัสให้)"}
              </button>
              <button
                type="button"
                className="secondary"
                onClick={() => {
                  setShowCreate(false);
                  setCreateErr(null);
                }}
                disabled={creating}
              >
                ยกเลิก
              </button>
            </div>

            {createErr && <div className="msg error">{createErr}</div>}

            {created && (
              <div className="msg ok">
                <b>
                  {created.identity === "created" ? "สร้างคลินิกแล้ว" : "คลินิกนี้มีอยู่แล้ว"} ·{" "}
                  <code>{created.clinic_code}</code>
                </b>
                {created.generated && <> (รหัสสุ่มให้)</>}
                <ul className="tight">
                  <li>
                    ตัวคลินิก: <b>{created.identity === "created" ? "สร้างใหม่" : "มีอยู่แล้ว"}</b>
                  </li>
                  <li>
                    สาขา:{" "}
                    <b>
                      {created.branch === "created"
                        ? `สร้างใหม่ ${created.branch_code ?? ""}`
                        : `มีอยู่แล้ว ${created.branch_count} สาขา`}
                    </b>
                    {created.branch === "created" && created.identity === "existed" && (
                      <>
                        {" "}
                        — คลินิกนี้เดิมไม่มีสาขา ซึ่งทำให้บันทึกคนไข้ไม่ได้ ตอนนี้แก้แล้ว
                      </>
                    )}
                  </li>
                  <li>
                    subscription: <b>{created.subscription}</b>
                    {created.subscription_status ? <> ({created.subscription_status})</> : null} ·{" "}
                    {created.never_expires ? (
                      <b style={{ color: "var(--ok)" }}>ไม่มีวันหมดอายุ</b>
                    ) : (
                      <b style={{ color: "var(--bad)" }}>หมดอายุ {created.expires_on}</b>
                    )}{" "}
                    · ฟีเจอร์: <b>{created.features}</b>
                  </li>
                </ul>

                {!created.never_expires && (
                  <div className="msg warn" style={{ margin: "8px 0" }}>
                    คลินิกนี้มี subscription เดิมอยู่แล้วและ<b>ยังนับถอยหลังถึง {created.expires_on}</b> —
                    ระบบไม่เขียนทับให้ เพราะใบเดิมอาจเป็นแพ็กเกจที่จ่ายเงินอยู่ ถ้าต้องการให้ไม่หมดอายุ
                    ต้องแก้ที่ระบบ V4 เอง
                  </div>
                )}

                {created.owner ? (
                  <div style={{ marginTop: 8 }}>
                    <b>บัญชี owner</b>
                    <ul className="tight">
                      <li>
                        <code>{created.owner.email}</code> · {created.owner.name} (user_id{" "}
                        {created.owner.user_id})
                      </li>
                      <li>
                        บัญชี: <b>{created.owner.user === "created" ? "สร้างใหม่" : "มีอยู่แล้ว"}</b> ·
                        สิทธิ์ owner: <b>{created.owner.membership === "created" ? "ผูกให้แล้ว" : "มีอยู่แล้ว"}</b> ·
                        ทะเบียนพนักงาน: <b>{created.owner.staff === "created" ? "สร้างใหม่" : "มีอยู่แล้ว"}</b>
                      </li>
                      {created.owner.clinic_already_had_owner && created.owner.user === "existed" && (
                        <li className="muted">คลินิกนี้มี owner อยู่ก่อนแล้ว ระบบไม่ได้แตะรหัสผ่านเดิม</li>
                      )}
                    </ul>

                    {created.owner.generated_password ? (
                      <div className="tgt-danger" role="alert">
                        <strong>รหัสผ่านนี้แสดงครั้งเดียว</strong> — คัดลอกเก็บไว้เดี๋ยวนี้ ระบบเก็บไว้เป็น
                        bcrypt hash เท่านั้น เปิดดูซ้ำไม่ได้
                        <br />
                        <code style={{ fontSize: 15, letterSpacing: 1 }}>
                          {created.owner.generated_password}
                        </code>
                        {created.owner.must_change_password && (
                          <>
                            <br />
                            ต้องเปลี่ยนรหัสผ่านตอนล็อกอินครั้งแรก
                          </>
                        )}
                      </div>
                    ) : created.owner.user === "created" ? (
                      <div className="muted" style={{ fontSize: 12 }}>
                        ใช้รหัสผ่านที่ส่งมาในคำขอ (ไม่แสดงซ้ำ)
                      </div>
                    ) : null}
                  </div>
                ) : ownerEmail.trim() ? null : (
                  <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
                    ยังไม่ได้สร้างบัญชี owner (ไม่ได้กรอกอีเมล) — คลินิกนี้จะยังไม่มีใครล็อกอินเข้าไปดูแลได้
                  </div>
                )}

                <div style={{ marginTop: 8 }}>
                  รหัสถูกเติมลงช่อง <code>clinic_code</code> ให้แล้ว — ไปขั้นถัดไปได้เลย
                </div>
              </div>
            )}
          </div>
        )}

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
            {!selected ? (
              <div className="msg warn">
                ปุ่มนี้ปิดอยู่ — ยังไม่ได้เลือกปลายทางที่ตั้งค่าครบ (ดูกล่อง 1 · ปลายทาง)
              </div>
            ) : (
              <>
                <div className={onProduction ? "tgt-danger" : "msg warn"} role={onProduction ? "alert" : undefined}>
                  {onProduction ? (
                    <>
                      <strong>กำลังจะเขียนลงเซิร์ฟเวอร์ของจริง</strong>
                      <br />
                    </>
                  ) : (
                    <>
                      <b>เขียนลงฐานข้อมูลจริง ย้อนยาก</b>
                      <br />
                    </>
                  )}
                  ปลายทาง: <code>{selected.description}</code>
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
                    className={`go${onProduction ? " is-prod" : ""}`}
                    onClick={importToServer}
                    disabled={importing || !clinic.trim() || serverConfirm.trim() !== clinic.trim()}
                  >
                    {importing
                      ? "กำลังนำเข้า…"
                      : onProduction
                        ? `import to ${selected.label} (ของจริง)`
                        : `import to ${selected.label}`}
                  </button>
                  {importing && (
                    <span className="muted">กำลังรัน psql บน {selected.label}…</span>
                  )}
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
