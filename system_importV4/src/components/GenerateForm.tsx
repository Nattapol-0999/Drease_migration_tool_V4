"use client";

import { Fragment, useEffect, useState } from "react";
import {
  CLI_INPUTS,
  IMPORT_GROUPS,
  CHUNK_BYTES,
  JOB_POLL_MS,
  MAX_UPLOAD_BYTES,
  MONGO_COLLECTIONS_USED,
  branchMapArg,
  type BranchSurvey,
  type BranchSurveyRow,
  type CliInputField,
  type StagedField,
  type JobStartResponse,
  type JobStatusResponse,
  type JobView,
  type ImportGroup,
  type LegacySourceStatus,
  type ProvisionBranchInput,
  type PullResponse,
  type SurveyBranchesResponse,
  type TaxProfileInput,
  type ClinicAPIStatus,
  type ClinicAPIStatusResponse,
  type CreateClinicResponse,
  type PhotoImportResponse,
  type ProvisionedClinic,
  type TargetId,
  type TargetInfo,
  type TargetsResponse,
} from "@/lib/types";

import BusyOverlay, { type BusyState } from "./BusyOverlay";

import "./target-picker.css";
import "./flow-steps.css";

/**
 * postFormWithProgress posts a FormData and reports how many bytes have left the browser.
 *
 * fetch() cannot do this — no browser implements an upload-progress event for it — so a
 * 240 MB body is indistinguishable from a hung request. XMLHttpRequest still exposes
 * `upload.onprogress`, and that is the only measured number available on this path.
 *
 * The response is handed back as raw text with its status: a rejected upload often comes
 * back as HTML from a proxy (413), and JSON.parse-ing that produces a syntax error
 * instead of the reason.
 */
function postFormWithProgress(
  url: string,
  body: FormData,
  onProgress: (loaded: number, total: number) => void,
  onUploaded: () => void,
): Promise<{ status: number; text: string }> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", url);
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress(e.loaded, e.total);
    };
    xhr.upload.onload = () => onUploaded();
    xhr.onload = () => resolve({ status: xhr.status, text: xhr.responseText });
    // Every failure path says what happened. A silent handler here is how a dropped
    // connection halfway through a large upload turns into a form that simply stops.
    xhr.onerror = () =>
      reject(
        new Error(
          "ส่งไฟล์ไม่สำเร็จ — การเชื่อมต่อขาดกลางทาง (ไฟล์ใหญ่เกินที่ proxy ยอมรับก็ขึ้นแบบนี้)",
        ),
      );
    xhr.ontimeout = () => reject(new Error("ส่งไฟล์ไม่สำเร็จ — หมดเวลารอ"));
    xhr.onabort = () => reject(new Error("การส่งไฟล์ถูกยกเลิก"));
    xhr.send(body);
  });
}

/**
 * uploadOneChunk posts a single slice and returns the session it belongs to.
 *
 * Slices go out one at a time rather than in parallel: the server appends them and refuses
 * anything that does not start exactly where the file currently ends, which is what makes
 * a scrambled dump impossible. Sequential is also kinder to a home upstream link.
 */
async function uploadOneChunk(
  sessionId: string,
  field: StagedField,
  offset: number,
  slice: Blob,
  onProgress: (loadedInSlice: number) => void,
): Promise<string> {
  const body = new FormData();
  if (sessionId) body.append("sessionId", sessionId);
  body.append("field", field);
  body.append("offset", String(offset));
  body.append("chunk", slice);

  const res = await postFormWithProgress(
    "/api/upload-chunk",
    body,
    (loaded) => onProgress(loaded),
    () => {},
  );

  let json: { ok: boolean; sessionId?: string; error?: string };
  try {
    json = JSON.parse(res.text) as typeof json;
  } catch {
    throw new Error(
      `ส่งชิ้นไฟล์แล้วได้คำตอบที่ไม่ใช่ JSON (HTTP ${res.status}) — ${
        res.text.trim().slice(0, 200) || "ไม่มีเนื้อหา"
      }`,
    );
  }
  if (!json.ok || !json.sessionId) {
    throw new Error(json.error ?? `ส่งชิ้นไฟล์ไม่สำเร็จ (HTTP ${res.status})`);
  }
  return json.sessionId;
}

/**
 * waitForJob polls a job until it finishes, reporting each phase change.
 *
 * Polling rather than one long request because Cloudflare closes a proxied request that
 * has not answered in 100 seconds, and this work takes minutes. Every poll is a few bytes,
 * so the cost of asking often is nothing.
 */
async function waitForJob(jobId: string, onTick: (job: JobView) => void): Promise<JobView> {
  for (;;) {
    await new Promise((r) => setTimeout(r, JOB_POLL_MS));

    const res = await fetch(`/api/job?jobId=${encodeURIComponent(jobId)}`, { cache: "no-store" });
    const text = await res.text();
    let json: JobStatusResponse;
    try {
      json = JSON.parse(text) as JobStatusResponse;
    } catch {
      throw new Error(
        `ถามสถานะงานแล้วได้คำตอบที่ไม่ใช่ JSON (HTTP ${res.status}) — ${
          text.trim().slice(0, 200) || "ไม่มีเนื้อหา"
        }`,
      );
    }
    if (!json.ok || !json.job) {
      throw new Error(json.error ?? `ถามสถานะงานไม่สำเร็จ (HTTP ${res.status})`);
    }

    onTick(json.job);
    if (json.job.state === "running") continue;
    if (json.job.state === "failed") {
      const detail = json.job.log.length ? `\n\n${json.job.log.join("\n")}` : "";
      throw new Error((json.job.error ?? "งานล้มเหลว") + detail);
    }
    return json.job;
  }
}

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

/*
 * THERE IS NO SUBSCRIPTION PLAN PICKER HERE, AND THAT IS THE POINT.
 *
 * One briefly existed. It was not added because anyone wanted this screen to sell anything —
 * it was added to get past a refusal. Provisioning read its branch ceiling from the plan
 * named in the request, defaulting to standard's three, so a seven-branch import came back
 * 422 branch_cap_exceeded. A dropdown was the shortest way to make the request pass, and it
 * turned a data-migration screen into the place where a monthly charge got chosen, by
 * whoever happened to be running the import.
 *
 * The branch count was never a question this screen needed to ask. It is read out of the
 * legacy `branch` table in step 2 and it is a fact about the clinic, not a request for
 * entitlement. The fix belonged in the backend, and that is where it went: when a caller
 * names no plan, provisioning takes the branch count as given and bounds it only by the two
 * digits branch_no can hold (admin/provision.go, ensureBranches). The plan cap still applies
 * in full to callers that DO name one, and Settings still enforces it on everything the
 * clinic does itself.
 *
 * So this form sends no tier. The clinic gets the same default subscription it always got,
 * and whoever sells the plan sets it afterwards.
 */

/**
 * howMuchDataNamesThisBranch is the number that tells an operator a branch is real.
 *
 * The legacy branch table happily holds rows nobody ever used, and it holds departments
 * that are not clinics at all (a marketing desk, an online-service desk). The counts are
 * the only honest way to tell those apart, so they are added up and shown rather than the
 * operator being asked to recognise names.
 */
function branchDataCount(b: BranchSurveyRow): number {
  return (
    b.patients + b.appointments + b.rooms + b.bills + b.receipts + b.courses_bought + b.stock_lots
  );
}

/*
 * There is no sameTaxEntity() here any more.
 *
 * It answered "can these two branches live in the same V4 clinic" by comparing tax ids, and
 * the pre-selection used it to tick only the main branch's own company. Since 11 Sep 2026
 * every branch is ticked and the header carries the majority company's number, so nothing
 * decides membership by tax id — the form names the mismatched branches instead of excluding
 * them. Keeping the helper would leave the old policy sitting in the file, readable as though
 * it were still in force.
 *
 * The comparison itself was right about one thing worth remembering: match on the TAX ID, not
 * the company name. Names in these exports are typed by hand and disagree with themselves —
 * one tenant spelled the same company three ways across its branches — so a name comparison
 * splits one company apart while missing the branches that really do belong to another. That
 * is why majorityTaxID and branchesUnderAnotherTaxID both key on the number.
 */

/** A stable identity for the attached files, so a swapped file invalidates the upload. */
function filesSignature(files: Partial<Record<CliInputField, File>>): string {
  return CLI_INPUTS.map((i) => {
    const f = files[i.field];
    return f ? `${i.field}:${f.name}:${f.size}:${f.lastModified}` : `${i.field}:-`;
  }).join("|");
}

export default function GenerateForm() {
  const [clinic, setClinic] = useState("");
  const [owner, setOwner] = useState("");
  const [files, setFiles] = useState<Partial<Record<CliInputField, File>>>({});
  const [busy, setBusy] = useState(false);
  // Non-null while "สร้างไฟล์ .sql" is in flight; drives the full-screen overlay.
  const [busyState, setBusyState] = useState<BusyState | null>(null);
  const [err, setErr] = useState<string | null>(null);
  // The generated script stays on the server: it can be hundreds of megabytes, so the
  // browser holds only its identity, size and head.
  const [result, setResult] = useState<{
    jobId: string;
    filename: string;
    sqlBytes: number;
    preview: string;
    report: string;
    log: string[];
  } | null>(null);

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
  // ── สาขา ────────────────────────────────────────────────────────────────────
  //
  // The upload now happens BEFORE the clinic is created, because the branch list has to be
  // read out of the dump first: V4 allocates its own branch codes, so the legacy "01" can
  // only be translated after both sides exist. `sessionId` is that upload, kept so the
  // generate step does not send a multi-gigabyte file twice.
  const [sessionId, setSessionId] = useState("");
  const [uploadedBytes, setUploadedBytes] = useState(0);
  const [surveying, setSurveying] = useState(false);
  const [surveyErr, setSurveyErr] = useState<string | null>(null);
  const [survey, setSurvey] = useState<BranchSurvey | null>(null);
  const [surveySeconds, setSurveySeconds] = useState("");
  /** Legacy branch codes the operator wants this clinic to hold. */
  const [picked, setPicked] = useState<string[]>([]);
  /** The legacy code of the main branch — also the destination for unresolvable rows. */
  const [mainCode, setMainCode] = useState("");
  const [importing, setImporting] = useState(false);
  const [checking, setChecking] = useState(false);
  // Everything by default; an operator narrows it when the target already has some.
  const [groups, setGroups] = useState<ImportGroup[]>(IMPORT_GROUPS.map((g) => g.group));
  const [importErr, setImportErr] = useState<string | null>(null);
  const [importLog, setImportLog] = useState<string | null>(null);
  const [importDone, setImportDone] = useState<string | null>(null);

  // ── รูปหน้าเวชระเบียนจากระบบเก่า ────────────────────────────────────────────
  // Lives inside box 2 because it reads the SAME MongoDB (.js) file the catalog does —
  // one attachment, two things read out of it. Only the archive of image bytes is its
  // own input, because that is not a dump and cannot come from the same place.
  const [imgFile, setImgFile] = useState<File | null>(null);
  const [photoBusy, setPhotoBusy] = useState<"" | "survey" | "apply">("");
  const [photoSurvey, setPhotoSurvey] = useState<PhotoImportResponse | null>(null);
  const [photoErr, setPhotoErr] = useState<string | null>(null);
  const [photoDone, setPhotoDone] = useState<string | null>(null);
  const [photoLog, setPhotoLog] = useState<string | null>(null);
  const [photoConfirm, setPhotoConfirm] = useState("");
  // The upload the photo step's two files were put in, kept so survey → apply does not
  // send 530 MB twice. Cleared whenever either file changes (see the effect below), so a
  // reused session can only ever hold the files currently attached.
  const [photoSessionId, setPhotoSessionId] = useState("");

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
  const chosenBytes = chosen.reduce((n, i) => n + (files[i.field]?.size ?? 0), 0);
  const tooBig = chosenBytes > MAX_UPLOAD_BYTES;
  const selected = targets?.find((t) => t.id === targetId) ?? null;
  const onProduction = selected?.isProduction === true;
  const createApi = clinicApi?.find((c) => c.id === targetId) ?? null;

  // ── สาขาที่จะสร้าง ──────────────────────────────────────────────────────────
  const surveyRows = survey?.branches ?? [];
  const pickedRows = surveyRows.filter((b) => picked.includes(b.code));
  const mainRow = surveyRows.find((b) => b.code === mainCode) ?? null;
  /** Legacy codes present in the DATA but not in the branch table — typos, a few rows. */
  const strayCodes = Object.entries(survey?.unknown_in_data ?? {});
  /** The map the CLI will be given, straight from what the backend actually created. */
  const branchMap = created?.branches ? branchMapArg(created.branches) : "";
  const mainBranchCode =
    created?.branches?.find((b) => b.is_main)?.branch_code ??
    created?.branches?.find((b) => (b.source_code ?? "") === mainCode)?.branch_code ??
    "";
  /**
   * A swapped or cleared file makes the upload on the server, and every answer derived
   * from it, describe a file that is no longer attached. Both are dropped rather than
   * left to look current — a branch list read from the previous dump is exactly the kind
   * of stale answer that gets 14,000 patients filed at the wrong place.
   */
  const filesSig = filesSignature(files);
  useEffect(() => {
    setSessionId("");
    setUploadedBytes(0);
    setSurvey(null);
    setSurveyErr(null);
    setPicked([]);
    setMainCode("");
  }, [filesSig]);

  /**
   * uploadAll sends every attached dump to the staging directory and returns the session.
   *
   * Extracted from the generate step because the branch list has to be read on the server
   * before the clinic is created, and that means the bytes have to be there first. Sending
   * them once and keeping the session id is the whole point: these files reach gigabytes,
   * and uploading them twice for two questions about the same dump is not a rounding error.
   *
   * Slices go out one at a time; the server refuses a chunk that does not begin exactly
   * where the file currently ends, which is what makes a scrambled dump impossible.
   */
  async function uploadAll(title: string): Promise<string> {
    const totalBytes = chosenBytes;
    const chunkCount = chosen.reduce(
      (n, i) => n + Math.ceil((files[i.field]?.size ?? 0) / CHUNK_BYTES),
      0,
    );

    setBusyState({
      title,
      phase: `กำลังส่งไฟล์ขึ้นเซิร์ฟเวอร์ (ชิ้น 1/${chunkCount})`,
      percent: 0,
      detail: `0 B / ${human(totalBytes)}`,
      startedAt: Date.now(),
      note: "ห้ามปิดแท็บหรือกดรีเฟรชระหว่างนี้ — ไฟล์กำลังส่งอยู่",
    });

    let sid = "";
    let sentBytes = 0;
    let chunkNo = 0;
    for (const { field } of CLI_INPUTS) {
      const f = files[field];
      if (!f) continue;
      for (let offset = 0; offset < f.size; offset += CHUNK_BYTES) {
        const slice = f.slice(offset, Math.min(offset + CHUNK_BYTES, f.size));
        chunkNo++;
        const base = sentBytes;
        const label = `กำลังส่งไฟล์ขึ้นเซิร์ฟเวอร์ (ชิ้น ${chunkNo}/${chunkCount})`;
        sid = await uploadOneChunk(sid, field, offset, slice, (loadedInSlice) =>
          setBusyState((s) =>
            s
              ? {
                  ...s,
                  phase: label,
                  percent: ((base + loadedInSlice) / totalBytes) * 100,
                  detail: `${human(base + loadedInSlice)} / ${human(totalBytes)}`,
                }
              : s,
          ),
        );
        sentBytes += slice.size;
      }
    }

    setSessionId(sid);
    setUploadedBytes(sentBytes);
    return sid;
  }

  /**
   * uploadAndSurvey puts the dumps on the server and reads the legacy branch table.
   *
   * WHY THIS IS THE FIRST STEP AND NOT AN OPTION
   * A V2 tenant can have many branches — one export had nine — and every patient, bill,
   * room and stock lot in it names the one it belongs to. V4 will not accept those names:
   * it allocates its own branch codes, each carrying a branch_no that the HNs issued at
   * that branch keep for life. So the legacy code has to be TRANSLATED, the translation
   * table can only be built once both sides exist, and the V4 side can only be built once
   * somebody has seen what the export contains. Reading the table first is not a
   * convenience, it is the only order that works.
   *
   * Nothing is written anywhere by this: the CLI's survey mode takes no clinic and produces
   * no script.
   */
  async function uploadAndSurvey() {
    setSurveyErr(null);
    setSurvey(null);
    setResult(null);
    if (chosen.length === 0) {
      setSurveyErr("เลือกไฟล์ dump อย่างน้อยหนึ่งไฟล์ก่อน");
      return;
    }
    if (!files.sql) {
      setSurveyErr(
        "ตารางสาขา (branch) อยู่ในไฟล์ MySQL — ต้องแนบช่อง MySQL (.sql) ก่อน จะอ่านสาขาจากไฟล์ MongoDB ไม่ได้",
      );
      return;
    }
    if (tooBig) {
      setSurveyErr(
        `ไฟล์รวมกัน ${human(chosenBytes)} เกินเพดาน ${human(MAX_UPLOAD_BYTES)} — ยังไม่ได้ส่งอะไรออกไป`,
      );
      return;
    }

    setSurveying(true);
    try {
      const sid = await uploadAll("อ่านสาขาจาก dump");

      setBusyState((s) =>
        s
          ? {
              ...s,
              phase: "กำลังอ่านตาราง branch จากไฟล์ MySQL",
              percent: null,
              detail: `ส่งครบแล้ว ${human(chosenBytes)}`,
              note: "อ่านอย่างเดียว ไม่เขียนอะไรทั้งที่นี่และที่ปลายทาง",
            }
          : s,
      );

      // A job, not an inline answer. With the Mongo half attached this reads a 921 MB export
      // and took 8m01s on the deployed box, well past the 100 seconds a proxied request may
      // stay open. The reason to pay that: the counts that say whether a branch is real —
      // its lots, bills, receipts, courses — are all in the Mongo file, and it was not being
      // passed at all, so they read 0 on screen with nothing to show they were never looked
      // up. A branch with 531 lots looked like a branch holding nothing.
      const startRes = await fetch("/api/survey-branches", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ sessionId: sid }),
      });
      const startText = await startRes.text();
      let start: { ok?: boolean; jobId?: string; error?: string };
      try {
        start = JSON.parse(startText) as typeof start;
      } catch {
        setSurveyErr(
          `เซิร์ฟเวอร์ตอบไม่ใช่ JSON (HTTP ${startRes.status}) — ${
            startText.trim().slice(0, 300) || "ไม่มีเนื้อหา"
          }`,
        );
        return;
      }
      if (!start.ok || !start.jobId) {
        setSurveyErr(start.error ?? `เริ่มงานสำรวจไม่สำเร็จ (HTTP ${startRes.status})`);
        return;
      }

      const done = await waitForJob(start.jobId, (job) =>
        setBusyState((s) =>
          s
            ? {
                ...s,
                phase: job.phase,
                percent: null,
                detail: `${job.seconds} วินาที`,
                note: "อ่านอย่างเดียว ไม่เขียนอะไรทั้งที่นี่และที่ปลายทาง",
              }
            : s,
        ),
      );
      if (done.state !== "done") {
        setSurveyErr(done.error || "อ่านสาขาไม่สำเร็จ");
        return;
      }
      const surveyed = done.survey;
      if (!surveyed || !Array.isArray(surveyed.branches)) {
        setSurveyErr("งานสำรวจเสร็จแล้วแต่ไม่มีผลกลับมา — ดู log ของเซิร์ฟเวอร์");
        return;
      }
      const json = { survey: surveyed };

      setSurvey(json.survey);
      setSurveySeconds(done.seconds);

      // EVERY branch the registry lists is ticked. The tenant's branches are a fact about
      // the tenant, and a branch left out has its patients, rooms and bills land on some
      // other branch — or not land at all — which is a bigger problem than the one the old
      // pre-selection was avoiding.
      //
      // What it was avoiding: clinic_settings holds ONE tax id, so branches belonging to a
      // second company print that company's number on their receipts. The earlier default
      // ticked only the main branch's own company and left the others for a separate import.
      // Decided 11 Sep 2026 to take them all instead, with the majority company's number on
      // the header — the operator is told below exactly which branches that misnames, and
      // can still untick them to do it the other way.
      const rows = json.survey.branches ?? [];
      if (rows.length > 0) {
        const main = rows.reduce((a, b) => (branchDataCount(b) > branchDataCount(a) ? b : a));
        setMainCode(main.code);
        setPicked(rows.map((b) => b.code));
      }
    } catch (e) {
      setSurveyErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSurveying(false);
      setBusyState(null);
    }
  }

  /** The branch list to provision, in survey order so the result is reproducible. */
  function branchesToCreate(): ProvisionBranchInput[] {
    return pickedRows.map((b) => ({
      source_code: b.code,
      name: b.name || b.code,
      name_en: b.name_en,
      address: b.address,
      phone: b.phone,
      is_main: b.code === mainCode,
    }));
  }

  /**
   * majorityTaxID returns the tax id this clinic's documents will carry.
   *
   * The main branch's own number wins, because every other field on the header — legal name,
   * address, phone — already comes from that row, and taking the number from a different
   * branch would print one company's TIN above another's address.
   *
   * When the main branch recorded none, the most common number among the ticked branches is
   * used instead: with several branches under one company and a handful that simply never
   * filled the field in, that company is the answer. Ties break on the lowest number so two
   * runs over one file always produce the same header.
   *
   * Blanks never count. A missing number means nobody typed it, not that a branch belongs to
   * a company with no TIN.
   */
  function majorityTaxID(): string {
    const own = (mainRow?.tax_id ?? "").trim();
    if (own !== "") return own;

    const tally = new Map<string, number>();
    for (const b of pickedRows) {
      const id = b.tax_id.trim();
      if (id === "") continue;
      tally.set(id, (tally.get(id) ?? 0) + 1);
    }
    let best = "";
    let bestCount = 0;
    for (const [id, count] of [...tally.entries()].sort((a, b) => a[0].localeCompare(b[0]))) {
      if (count > bestCount) {
        best = id;
        bestCount = count;
      }
    }
    return best;
  }

  /** Ticked branches whose own tax id differs from the one the header will carry. */
  function branchesUnderAnotherTaxID(): BranchSurveyRow[] {
    const header = majorityTaxID();
    if (header === "") return [];
    return pickedRows.filter((b) => {
      const id = b.tax_id.trim();
      return id !== "" && id !== header;
    });
  }

  /**
   * taxProfileToSend takes the receipt header from the MAIN branch's own row.
   *
   * tax_id used to be left EMPTY on purpose (9 Sep 2026): the export's numbers are not
   * consistent — one tenant carried three companies and four branches with no number at all
   * — and a wrong number on a tax document is a legal problem, not a data problem, so a human
   * was left to type it.
   *
   * Reversed 11 Sep 2026: the majority company's number is written now. What has not changed
   * is who carries the risk of it being wrong for a minority branch, so the form names those
   * branches on screen before the clinic is created. The backend still writes this profile
   * only into a clinic that has none, so a correction made in Settings survives a re-run.
   */
  function taxProfileToSend(): TaxProfileInput | null {
    if (!mainRow) return null;
    const legal = (mainRow.legal_name || mainRow.name || "").trim();
    if (!legal) return null;
    return {
      legal_name: legal,
      tax_id: majorityTaxID(),
      tax_branch_type: "",
      tax_branch_code: "",
      tax_address: mainRow.address,
      phone: mainRow.phone,
      email: mainRow.email,
      website: "",
      receipt_footer: "",
      logo_url: "",
    };
  }

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
          // No tier: naming a plan here would make this screen choose a monthly charge, and
          // the branch count it would be chosen to accommodate is already a fact read out of
          // the legacy branch table. See the note at the top of this file.
          // Sent only when the branch table was actually read and branches chosen. An empty
          // list is omitted entirely so the backend keeps its own "one branch named after
          // the clinic" default instead of interpreting a blank.
          branches: pickedRows.length > 0 ? branchesToCreate() : undefined,
          taxProfile: pickedRows.length > 0 ? taxProfileToSend() : undefined,
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

  // Swapping either file invalidates the photo survey: the plan was derived from the old
  // file's `page` collection and weighed against the old archive, and offering an apply
  // built on a file that is no longer attached is how the wrong images get written.
  //
  // The staged upload is dropped with it. It is keyed on nothing but this state, so a
  // session held past a file change would be the previous bytes wearing the new file's
  // name — the exact confusion the survey reset exists to prevent.
  const mongoName = files.mongo?.name ?? "";
  const mongoSize = files.mongo?.size ?? 0;
  const imgName = imgFile?.name ?? "";
  const imgSize = imgFile?.size ?? 0;
  useEffect(() => {
    setPhotoSurvey(null);
    setPhotoDone(null);
    setPhotoErr(null);
    setPhotoLog(null);
    setPhotoSessionId("");
  }, [mongoName, mongoSize, imgName, imgSize]);

  /**
   * runPhotoImport calls the one route in either mode.
   *
   * The two files go up CHUNKED, then the mode call carries only the session id. They used
   * to ride in this request, which held while both were small — the first clinic's
   * 28 MB dump + 25.6 MB archive fit under the 64 MB body cap with room to spare. The next
   * one did not: 437 photos came to 520 MB, and nginx (64 MB) and Cloudflare (100 MB) both
   * sit in front of this app. The staging directory has no such ceiling.
   *
   * The plan is still rebuilt from the dump on every call rather than cached between
   * survey and apply: what the operator confirmed is what runs, even if a file changed in
   * between. Re-uploading is what is avoided here, not re-planning.
   */
  /**
   * uploadPhotoInputs puts the Mongo dump and the image archive on the server in slices and
   * answers with the session that holds them.
   *
   * The Mongo dump goes to the `mongo` field — the same one the SQL import uses — because it
   * IS the same file: the catalog and the `page` collection come out of one export. The
   * archive goes to `images`, which exists only for this step.
   *
   * A session already built for these exact files is reused, so a survey followed by an
   * apply sends the bytes once. The effect above drops it the moment either file changes.
   */
  async function uploadPhotoInputs(dump: File, images: File): Promise<string> {
    if (photoSessionId) return photoSessionId;

    const parts: { field: StagedField; file: File }[] = [
      { field: "mongo", file: dump },
      { field: "images", file: images },
    ];
    const totalBytes = parts.reduce((n, p) => n + p.file.size, 0);
    const chunkCount = parts.reduce((n, p) => n + Math.ceil(p.file.size / CHUNK_BYTES), 0);

    setBusyState({
      title: "นำเข้ารูปหน้าเวชระเบียน",
      phase: `กำลังส่งไฟล์ขึ้นเซิร์ฟเวอร์ (ชิ้น 1/${chunkCount})`,
      percent: 0,
      detail: `0 B / ${human(totalBytes)}`,
      startedAt: Date.now(),
      note: "ห้ามปิดแท็บหรือกดรีเฟรชระหว่างนี้ — ไฟล์กำลังส่งอยู่",
    });

    let sid = "";
    let sentBytes = 0;
    let chunkNo = 0;
    try {
      for (const { field, file } of parts) {
        for (let offset = 0; offset < file.size; offset += CHUNK_BYTES) {
          const slice = file.slice(offset, Math.min(offset + CHUNK_BYTES, file.size));
          chunkNo++;
          const base = sentBytes;
          const label = `กำลังส่งไฟล์ขึ้นเซิร์ฟเวอร์ (ชิ้น ${chunkNo}/${chunkCount})`;
          sid = await uploadOneChunk(sid, field, offset, slice, (loadedInSlice) =>
            setBusyState((s) =>
              s
                ? {
                    ...s,
                    phase: label,
                    percent: ((base + loadedInSlice) / totalBytes) * 100,
                    detail: `${human(base + loadedInSlice)} / ${human(totalBytes)}`,
                  }
                : s,
            ),
          );
          sentBytes += slice.size;
        }
      }
    } finally {
      setBusyState(null);
    }

    setPhotoSessionId(sid);
    return sid;
  }

  async function runPhotoImport(mode: "survey" | "apply") {
    const dump = files.mongo;
    if (!dump || !imgFile) return;
    setPhotoErr(null);
    setPhotoLog(null);
    if (mode === "survey") {
      setPhotoSurvey(null);
      setPhotoDone(null);
    }
    setPhotoBusy(mode);
    try {
      const sid = await uploadPhotoInputs(dump, imgFile);

      const body = new FormData();
      body.append("mode", mode);
      body.append("targetId", targetId);
      body.append("clinicCode", clinic.trim());
      body.append("sessionId", sid);
      if (mode === "apply") body.append("confirm", photoConfirm.trim());

      setPhotoBusy(mode);
      const res = await fetch("/api/import-photos", { method: "POST", body });
      const json = (await res.json()) as PhotoImportResponse;

      // The staging sweep drops a session after six hours, and a redeploy clears the whole
      // directory. Both surface as 409 here. Forgetting the id turns the next click into a
      // fresh upload instead of a second identical failure.
      if (res.status === 409) {
        setPhotoSessionId("");
        // The response is a union and a 409 is always the failing arm, but the narrowing
        // has to be written out for the type to admit it.
        const why = "error" in json ? json.error : "ไฟล์ที่อัปโหลดไว้หายไป";
        setPhotoErr(`${why} — กดอีกครั้งเพื่อส่งไฟล์ขึ้นใหม่`);
        return;
      }

      if (!json.ok) {
        setPhotoErr(json.error);
        if (json.log) setPhotoLog(json.log);
        // A failed apply still carries the counts, which is how an operator learns that
        // the bytes went over even though the rows did not.
        if (json.planned !== undefined) setPhotoSurvey(json as PhotoImportResponse);
        return;
      }
      if (json.mode === "survey") {
        setPhotoSurvey(json);
        return;
      }
      setPhotoSurvey(json);
      setPhotoDone(
        `นำเข้าแล้ว · คัดลอก ${json.filesSent} ไฟล์ (${(json.bytesSent / 1024 / 1024).toFixed(1)} MB) → ${json.uploads} · เขียน ${json.rows} แถว → ${json.target} (${json.seconds}s)`,
      );
      setPhotoLog(json.log);
    } catch (e) {
      setPhotoErr(e instanceof Error ? e.message : String(e));
    } finally {
      setPhotoBusy("");
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
    // Checked before anything is sent. This ceiling is about staging disk, not the proxy:
    // the slices below are each small enough for Cloudflare and nginx.
    if (chosenBytes > MAX_UPLOAD_BYTES) {
      setErr(
        `ไฟล์รวมกัน ${human(chosenBytes)} เกินเพดาน ${human(MAX_UPLOAD_BYTES)} — ยังไม่ได้ส่งอะไรออกไป`,
      );
      return;
    }

    // Refusing here rather than importing a single-branch clinic by accident. The stray
    // codes and the unpicked branches are both real data that would land at the default
    // branch, and a run that files them silently looks exactly like a successful one.
    if (survey && survey.has_branch_table && surveyRows.length > 1 && !branchMap) {
      setErr(
        `dump นี้มี ${surveyRows.length} สาขา แต่ยังไม่มีแผนที่สาขา — ต้องสร้างคลินิกพร้อมสาขาในกล่อง 3 ก่อน ` +
          "ไม่งั้นทุกแถวจะถูกเขียนไปสาขาเดียว และคนไข้ทั้งหมดจะไปอยู่สาขาที่ไม่ใช่ของตัวเอง",
      );
      return;
    }

    const totalBytes = chosenBytes;

    setBusy(true);
    // The dumps are usually already on the server: the branch step put them there and kept
    // the session. Re-sending gigabytes to ask a second question about the same file is
    // avoided, but a session that was never opened (single-branch clinic, survey skipped)
    // or was invalidated by a swapped file still has to be uploaded now.
    let sid = sessionId;
    try {
      if (!sid) {
        sid = await uploadAll("สร้างไฟล์ .sql");
      } else {
        setBusyState({
          title: "สร้างไฟล์ .sql",
          phase: "ใช้ไฟล์ที่อัปโหลดไว้แล้วตอนอ่านสาขา — ไม่ต้องส่งซ้ำ",
          percent: null,
          detail: `บนเซิร์ฟเวอร์แล้ว ${human(uploadedBytes || totalBytes)}`,
          startedAt: Date.now(),
          note: "ห้ามปิดแท็บหรือกดรีเฟรชระหว่างนี้",
        });
      }

      // From here the server filters the dump and runs the CLI, neither of which reports
      // progress, so the number goes away rather than being invented.
      setBusyState((s) =>
        s
          ? {
              ...s,
              phase: "เซิร์ฟเวอร์กำลังกรอง collection ที่ไม่ใช้ออก แล้วสร้างสคริปต์ด้วย CLI",
              percent: null,
              detail: `ไฟล์บนเซิร์ฟเวอร์ ${human(uploadedBytes || totalBytes)}`,
              note: "ขั้นนี้ไม่มีเปอร์เซ็นต์ — การกรองและ CLI ไม่รายงานความคืบหน้าออกมา ตัวเลขที่ปลอมขึ้นมาจะหลอกตัวเอง",
            }
          : s,
      );

      // Starts the work and returns an id. Waiting for the result in one request would be
      // cut off by Cloudflare at 100 seconds; a real clinic takes minutes.
      const res = await fetch("/api/generate", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          sessionId: sid,
          clinicCode: clinic.trim(),
          owner: owner.trim(),
          // Only send `only` when it is a real subset; empty means everything.
          only:
            groups.length > 0 && groups.length < IMPORT_GROUPS.length ? groups.join(",") : "",
          // Built from what the backend actually created, never from what was requested:
          // the branch codes are the server's to assign.
          branchMap,
          defaultBranch: branchMap ? mainBranchCode : "",
        }),
      });
      const text = await res.text();

      let start: JobStartResponse;
      try {
        start = JSON.parse(text) as JobStartResponse;
      } catch {
        // Not JSON: a proxy or the platform answered instead of the route. Show the
        // status and what came back, because that names the real limit that was hit.
        setErr(
          `เซิร์ฟเวอร์ตอบไม่ใช่ JSON (HTTP ${res.status}) — ${
            text.trim().slice(0, 300) || "ไม่มีเนื้อหา"
          }`,
        );
        return;
      }
      if (!start.ok || !start.jobId) {
        setErr(start.error ?? `เริ่มงานไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }

      const done = await waitForJob(start.jobId, (job) =>
        setBusyState((s) =>
          s
            ? {
                ...s,
                phase: job.phase,
                percent: null,
                detail: job.log.length ? job.log[job.log.length - 1].split("\n")[0] : s.detail,
              }
            : s,
        ),
      );

      setResult({
        jobId: done.id,
        filename: done.filename ?? `import_${clinic.trim()}.sql`,
        sqlBytes: done.sqlBytes ?? 0,
        preview: done.preview ?? "",
        report: done.report ?? "",
        log: done.log,
      });
      // The server deletes the staging session once the CLI has run, so the id it held is
      // no longer usable. Forgetting it here means a second attempt uploads again instead
      // of failing with "session not found", which would be true but unhelpful.
      //
      // The PHOTO session is a different upload and survives on purpose: /api/generate keeps
      // it when an archive is staged beside the dumps, so pressing import next runs the
      // photos without sending 763 MB a second time.
      setSessionId("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
      setBusyState(null);
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

  /**
   * download hands the browser a URL instead of building a Blob.
   *
   * The script never passes through this page: a Blob of a 500 MB script would have to
   * exist in the tab's memory first. The endpoint streams it from disk and the browser
   * writes it straight to the download folder.
   */
  function download() {
    if (!result) return;
    window.location.href = `/api/job/download?jobId=${encodeURIComponent(result.jobId)}`;
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

  /**
   * importToServer names the artifact by job id rather than sending it.
   *
   * The script is already on the server from generation; posting it back would move
   * hundreds of megabytes for no reason and put them in memory at both ends. psql reads it
   * from disk. Like generation this is a job, because psql on a large script runs longer
   * than the 100 seconds Cloudflare allows.
   */
  /**
   * importToServer runs the SQL and, when an archive is attached, the photos — one press.
   *
   * WHY THE PHOTOS RIDE ALONG
   * They used to be a separate survey-then-apply further up the page, and it was never
   * pressed: three tenants went in and not one got its images. So the archive is uploaded
   * with everything else and this button does both, in the order the data requires — SQL
   * first, because photos.branch_code is resolved from the patient's branch and every row is
   * keyed on an HN.
   *
   * The standalone photo step stays on the page for the case this cannot cover: the SQL
   * commits before the images copy, so "data in, images missing" is recoverable without
   * deleting the tenant.
   */
  async function importToServer() {
    if (!result) return;

    // 2ข: no archive is a legitimate choice, but a silent one is how a tenant ends up with
    // no images and nobody noticing for a week. Asked once, here, not enforced.
    const withPhotos = photoSessionId !== "" && imgFile !== null;
    if (!withPhotos) {
      const why = !imgFile
        ? "ยังไม่ได้แนบไฟล์รูป (public_img.tar.gz)"
        : "ไฟล์รูปยังไม่ได้อัปโหลดขึ้นเซิร์ฟเวอร์ — กด “ตรวจก่อน (อ่านอย่างเดียว)” ที่ส่วนรูปหนึ่งครั้ง";
      if (
        !window.confirm(
          `${why}\n\nรอบนี้จะนำเข้าเฉพาะข้อมูล ไม่มีรูปหน้าเวชระเบียน\n\nกด OK เพื่อนำเข้าโดยไม่เอารูป · กด Cancel เพื่อกลับไปแนบไฟล์`,
        )
      ) {
        return;
      }
    }

    setImportErr(null);
    setImportLog(null);
    setImportDone(null);
    setImporting(true);
    setBusyState({
      title: `นำเข้าไปที่ ${selected?.label ?? targetId}`,
      phase: "กำลังเริ่ม",
      percent: null,
      detail: withPhotos
        ? `สคริปต์ ${human(result.sqlBytes)} + รูป ${human(imgFile?.size ?? 0)}`
        : `สคริปต์ ${human(result.sqlBytes)}`,
      startedAt: Date.now(),
      note: withPhotos
        ? "สคริปต์อยู่ในทรานแซกชันเดียว · รูปคัดลอกหลัง SQL ผ่านแล้ว ถ้ารูปล้มข้อมูลยังอยู่"
        : "สคริปต์อยู่ในทรานแซกชันเดียว ถ้ามีอะไรผิดฐานข้อมูลจะไม่ถูกเปลี่ยน",
    });
    try {
      const res = await fetch("/api/import-to-server", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          jobId: result.jobId,
          clinicCode: clinic.trim(),
          confirm: serverConfirm.trim(),
          targetId,
          // Empty means "SQL only" on the server side too, so the decision above is the
          // only place it is made.
          photoSessionId: withPhotos ? photoSessionId : "",
        }),
      });
      const text = await res.text();
      let start: JobStartResponse;
      try {
        start = JSON.parse(text) as JobStartResponse;
      } catch {
        setImportErr(
          `เซิร์ฟเวอร์ตอบไม่ใช่ JSON (HTTP ${res.status}) — ${
            text.trim().slice(0, 300) || "ไม่มีเนื้อหา"
          }`,
        );
        return;
      }
      if (!start.ok || !start.jobId) {
        setImportErr(start.error ?? `เริ่มนำเข้าไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }

      const done = await waitForJob(start.jobId, (job) =>
        setBusyState((s) => (s ? { ...s, phase: job.phase } : s)),
      );

      // The photo half reports separately, because its failure is not the import's failure.
      const photo = done.photo;
      const parts = [`นำเข้าสำเร็จ → ${done.target ?? targetId} (${done.seconds}s)`];
      if (photo?.ran && photo.ok) {
        parts.push(
          `รูป: คัดลอก ${photo.filesSent} ไฟล์ (${human(photo.bytesSent ?? 0)}) · เขียน ${photo.rows} แถว`,
        );
        // The upload was consumed by the server; a later press would find nothing.
        setPhotoSessionId("");
      }
      setImportLog([done.psqlLog, photo?.log].filter(Boolean).join("\n").trim() || null);
      setImportDone(parts.join(" · "));

      // Data in, images not: say so where the operator is looking, and point at the retry
      // instead of leaving them to work out that the tenant does not need deleting.
      if (photo && !photo.ok) {
        setImportErr(
          `ข้อมูลเข้าฐานเรียบร้อยแล้ว แต่รูปไม่เข้า: ${photo.message ?? "ไม่ทราบสาเหตุ"}` +
            (photo.ran
              ? " — ไฟล์รูปที่อัปโหลดไว้ยังอยู่ กดนำเข้ารูปซ้ำได้ที่ส่วน “รูปหน้าเวชระเบียน” ไม่ต้องนำเข้า SQL ใหม่"
              : ""),
        );
      }
    } catch (e) {
      setImportErr(e instanceof Error ? e.message : String(e));
    } finally {
      setImporting(false);
      setBusyState(null);
    }
  }

  // Each step is built as its own value and ordered in the return below, so the DOM order
  // is a single readable list instead of being implied by 1,600 lines of nesting. The
  // order matters: the dump has to be read before the clinic can be created, because the
  // branches it names are what the clinic has to be created WITH.
  const panelTarget = (
      <div className="panel">
        <h2>3 · ปลายทาง และสร้างคลินิก</h2>

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
              สร้างตัวคลินิก + สาขา + ฟีเจอร์ + subscription ในทรานแซกชันเดียว ·
              เรียกซ้ำด้วยรหัสเดิมไม่สร้างของซ้ำ (รายงานว่าอะไรมีอยู่แล้ว)
            </div>
            {pickedRows.length > 0 ? (
              <div className="msg info">
                จะสร้างพร้อม <b>{pickedRows.length} สาขา</b> ตามที่ติ๊กไว้ในกล่อง 2 · สาขาหลัก{" "}
                <code>{mainCode}</code> {mainRow ? `(${mainRow.name})` : ""}
                <br />
                ปลายทางจะคืน <b>รหัสสาขาของ V4</b> กลับมา ซึ่งเป็นค่าที่ขั้นสร้างสคริปต์ใช้เป็นแผนที่สาขา
              </div>
            ) : (
              <div className="msg warn">
                ยังไม่ได้อ่านสาขาในกล่อง 2 — จะสร้างเป็นคลินิก <b>สาขาเดียว</b>{" "}
                ถ้าคลินิกนี้มีหลายสาขาจริง ให้กลับไปกด &quot;อัปโหลด + อ่านสาขา&quot; ก่อน
              </div>
            )}

            <label htmlFor="new-clinic-name">ชื่อคลินิก (ไม่ใส่ก็ได้ — จะใช้ clinic_code เป็นชื่อ)</label>
            <input
              id="new-clinic-name"
              type="text"
              value={newThaName}
              onChange={(e) => setNewThaName(e.target.value)}
              placeholder="เช่น คลินิกตัวอย่าง"
            />

            {pickedRows.length > 0 && (
              <div className="muted" style={{ fontSize: 12, marginTop: 6 }}>
                จะสร้าง <b>{pickedRows.length} สาขา</b> ตามที่อ่านได้จากตาราง <code>branch</code>{" "}
                ในกล่อง 2 · แพ็กเกจ subscription ไม่ถูกตั้งจากหน้านี้
              </div>
            )}

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

  );

  const panelSource = (
      <div className="panel">
        <h2>1 · ที่มาของข้อมูล</h2>

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
          ช่อง <code>mongo</code> อ่าน <b>แคตตาล็อก</b> และ <b><code>page</code> (รูปหน้าเวชระเบียน)</b>{" "}
          — collection ฝั่งเงิน (<code>bills</code>, <code>payment_summary</code>,{" "}
          <code>course_bills</code>) อยู่ในไฟล์เดียวกันแต่ <b>ยังไม่ถูกนำเข้า</b>{" "}
          เพราะยังไม่ได้เคาะวิธีลงบัญชีย้อนหลัง
        </div>

        {/*
          รูปหน้าเวชระเบียน — อ่าน collection `page` จากไฟล์ MongoDB ช่องเดียวกันข้างบน
          ไม่ต้องแนบใหม่ ส่วนไบต์ของรูปมาเป็น .tar.gz แยก เพราะไม่ใช่ dump
        */}
        <div
          style={{
            marginTop: 14,
            paddingTop: 12,
            borderTop: "1px solid var(--border)",
          }}
        >
          <h3 style={{ marginTop: 0 }}>รูปหน้าเวชระเบียน (จาก collection <code>page</code>)</h3>
          <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
            ย้ายรูปใน <code>mris.page</code> ของ V2 มาเป็นรูปแนบในแฟ้มคนไข้ของ V4 (
            <code>photos</code>) · เอา <b>rev ล่าสุดของแต่ละหน้า</b> เท่านั้น ·
            รูปจะไปโผล่ในแฟ้ม OPD ของ HN นั้น
          </div>

          {/*
            ช่องแนบไฟล์รูปโชว์เสมอ ไม่ผูกกับว่าแนบ .js แล้วหรือยัง — สองไฟล์นี้ไม่ขึ้นกัน
            และช่องที่ถูกซ่อนคือช่องที่ผู้ใช้ไม่รู้ว่ามีอยู่ · เงื่อนไขไปอยู่ที่ปุ่มตรวจแทน
            ซึ่งบอกได้ด้วยว่ายังขาดอะไร
          */}
          <label htmlFor="img-tar">ไฟล์รูป (.tar.gz)</label>
          <input
            id="img-tar"
            type="file"
            accept=".gz,.tgz,.tar"
            onChange={(e) => {
              setImgFile(e.target.files?.[0] ?? null);
              setPhotoSurvey(null);
              setPhotoDone(null);
            }}
          />
          <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
            {imgFile ? (
              <>
                <span className="tag">แนบเอง</span> <code>{imgFile.name}</code> ·{" "}
                {human(imgFile.size)}
                <br />
              </>
            ) : null}
            <code>
              tar -czvf public_img.tar.gz -C /var/www/html/doctoreasetemporary/public/
              nas-doctorease/
            </code>
            <br />
            ต้องมีโฟลเดอร์ <code>nas-doctorease/</code> อยู่ในถุง · ไฟล์ใหญ่เท่าไรก็ได้ ระบบส่งขึ้นเป็นชิ้น
            เหมือนไฟล์ dump (คลินิกจริงวัดได้ 437 รูป = 530 MB)
          </div>

          {files.mongo ? (
            <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
              จะอ่าน <code>page</code> จาก <code>{files.mongo.name}</code> · ถ้าในไฟล์ไม่มี
              collection นี้ ระบบจะบอกว่าเจอ collection อะไรแทน
            </div>
          ) : (
            <div className="msg warn">
              ยังไม่ได้แนบไฟล์ <b>MongoDB (.js)</b> ในตารางข้างบน — ปุ่มตรวจจะเปิดเมื่อแนบแล้ว
              <br />
              <span className="muted" style={{ fontSize: 12 }}>
                <code>page</code> อยู่ในฐาน <code>mris</code> ซึ่ง <b>คนละฐานกับแคตตาล็อก</b>{" "}
                ฉะนั้นไฟล์เดียวนั้นต้องมีทั้งสองส่วน — ต่อท้าย export ของ <code>page</code>{" "}
                เข้าไปในไฟล์เดิมได้เลย ตัวอ่านแยก collection ให้เอง
                <br />
                <code>mongoexport --db=mris --collection=page {">>"} dbmongo.js</code>
                <br />
                หรือใช้ <code>scripts/export-v2-mongo.sh</code> บนเครื่องระบบเก่า —
                ดึงครบทั้ง {MONGO_COLLECTIONS_USED.length} collection ที่ระบบนี้อ่าน ลงไฟล์เดียว
                และไม่ลากของที่ไม่ใช้มาให้เกินเพดาน {human(MAX_UPLOAD_BYTES)}
              </span>
            </div>
          )}

          {!clinic.trim() ? (
            <div className="msg warn">
              กรอก <code>clinic_code</code> ในกล่อง 1 ก่อน — รูปต้องผูกกับคลินิกที่มีอยู่จริง
            </div>
          ) : (
            <>
              {(() => {
                // Say which attachment is still missing instead of a dead button with no
                // explanation. Both are needed: the dump names the images, the archive holds
                // their bytes.
                const need: string[] = [];
                if (!files.mongo) need.push("MongoDB (.js)");
                if (!imgFile) need.push("ไฟล์รูป (.tar.gz)");
                return (
                  <>
                    <div className="row" style={{ marginBottom: 4 }}>
                      <button
                        className="secondary"
                        type="button"
                        onClick={() => runPhotoImport("survey")}
                        disabled={photoBusy !== "" || need.length > 0}
                      >
                        {photoBusy === "survey" ? "กำลังตรวจ…" : "ตรวจก่อน (อ่านอย่างเดียว)"}
                      </button>
                      <span className="muted" style={{ fontSize: 12 }}>
                        {need.length > 0
                          ? `ยังขาด: ${need.join(" · ")}`
                          : "จับคู่รูปในถุงกับที่ dump บอก — ไม่คัดลอกไฟล์ ไม่เขียนฐาน"}
                      </span>
                    </div>
                  </>
                );
              })()}

                  {photoSurvey && photoSurvey.planned !== undefined && (
                    <div
                      className={
                        photoSurvey.missing && photoSurvey.missing.length > 0 ? "msg warn" : "msg ok"
                      }
                    >
                      <b>
                        dump บอกว่ามี {photoSurvey.planned} รูป · เจอในถุง {photoSurvey.matched} รูป
                        {photoSurvey.totalBytes !== undefined && (
                          <> ({(photoSurvey.totalBytes / 1024 / 1024).toFixed(1)} MB)</>
                        )}
                      </b>
                      <ul className="tight">
                        <li>
                          ไฟล์รูปจะไปที่ <code>{photoSurvey.uploads}</code>
                        </li>
                        <li>
                          แถวจะไปที่ <code>{photoSurvey.target}</code> — {photoSurvey.rows} แถว
                        </li>
                        {photoSurvey.extraCount ? (
                          <li>
                            ในถุงมีอีก {photoSurvey.extraCount} ไฟล์ที่ dump ไม่ได้อ้าง —
                            ไม่ถูกนำเข้า
                            {photoSurvey.extraSample && photoSurvey.extraSample.length > 0 && (
                              <>
                                {" "}
                                (เช่น <code>{photoSurvey.extraSample[0]}</code>)
                              </>
                            )}
                          </li>
                        ) : null}
                        {/* Not a warning: these ARE imported. It is here so nobody has to
                            work out later why a photo they can see is one they could not
                            upload today. */}
                        {photoSurvey.wouldRejectCount ? (
                          <li>
                            นำเข้าอยู่ แต่ปุ่มอัปโหลดในแอปวันนี้จะปฏิเสธ{" "}
                            {photoSurvey.wouldRejectCount} ไฟล์ (เกิน 3 MB{" "}
                            {photoSurvey.oversizeCount} · ไม่ใช่ JPG/PNG{" "}
                            {photoSurvey.unsupportedCount}) —{" "}
                            <span className="muted">
                              รูปที่หมอแนบไปแล้วยังอยู่ในเวชระเบียนและเปิดดูได้ปกติ
                            </span>
                          </li>
                        ) : null}
                        {photoSurvey.thumbFound !== undefined ? (
                          <li>
                            thumbnail จากถุงของ V2 เอง {photoSurvey.thumbFound} ไฟล์
                            {photoSurvey.thumbBytes ? (
                              <> ({(photoSurvey.thumbBytes / 1024 / 1024).toFixed(1)} MB)</>
                            ) : null}
                            {photoSurvey.matched !== undefined &&
                            photoSurvey.matched > photoSurvey.thumbFound ? (
                              <>
                                {" "}
                                · อีก {photoSurvey.matched - photoSurvey.thumbFound} ไฟล์ไม่มีคู่{" "}
                                <span className="muted">(ระบบใช้รูปจริงแทนเอง)</span>
                              </>
                            ) : null}
                          </li>
                        ) : null}
                        {photoSurvey.extMismatchCount ? (
                          <li>
                            นามสกุลไม่ตรงกับไบต์จริง {photoSurvey.extMismatchCount} ไฟล์ —{" "}
                            <span className="muted">
                              Content-Type ที่ส่งจะผิด แต่เบราว์เซอร์ตรวจเองจึงยังแสดงได้
                            </span>
                          </li>
                        ) : null}
                      </ul>
                      {photoSurvey.wouldReject && photoSurvey.wouldReject.length > 0 && (
                        <details>
                          <summary className="muted" style={{ cursor: "pointer", fontSize: 12 }}>
                            ดูรายการที่อัปโหลดใหม่ไม่ได้ ({photoSurvey.wouldRejectCount} ไฟล์)
                          </summary>
                          <pre
                            style={{
                              background: "var(--panel-2)",
                              border: "1px solid var(--border)",
                              borderRadius: 6,
                              padding: 10,
                              marginTop: 6,
                              overflowX: "auto",
                              fontSize: 11,
                              whiteSpace: "pre-wrap",
                              maxHeight: 200,
                            }}
                          >
                            {photoSurvey.wouldReject
                              .map((w) => `hn ${w.hn}  ${w.reason}`)
                              .join("\n")}
                            {/* The count is optional on the failure branch of the response
                                type, so it is defaulted rather than asserted. */}
                            {(photoSurvey.wouldRejectCount ?? 0) > photoSurvey.wouldReject.length
                              ? `\n… อีก ${(photoSurvey.wouldRejectCount ?? 0) - photoSurvey.wouldReject.length} รายการ`
                              : ""}
                          </pre>
                        </details>
                      )}
                      {photoSurvey.missing && photoSurvey.missing.length > 0 && (
                        <>
                          <b>
                            หาไฟล์ไม่เจอ {photoSurvey.missing.length} รูป — จะไม่ถูกนำเข้า
                            (แถวที่ชี้ไปไฟล์ที่ไม่มี = รูปเสียในเวชระเบียน)
                          </b>
                          <pre
                            style={{
                              background: "var(--panel-2)",
                              border: "1px solid var(--border)",
                              borderRadius: 6,
                              padding: 8,
                              maxHeight: 160,
                              overflow: "auto",
                              fontSize: 11,
                              whiteSpace: "pre-wrap",
                            }}
                          >
                            {photoSurvey.missing
                              .slice(0, 50)
                              .map((m) => `hn ${m.hn}  ${m.expected_path}`)
                              .join("\n")}
                            {photoSurvey.missing.length > 50
                              ? `\n… อีก ${photoSurvey.missing.length - 50} รายการ`
                              : ""}
                          </pre>
                        </>
                      )}
                    </div>
                  )}

                  {photoSurvey &&
                    photoSurvey.ok &&
                    photoSurvey.mode === "survey" &&
                    photoSurvey.matched > 0 && (
                      <>
                        <div
                          className={onProduction ? "tgt-danger" : "msg warn"}
                          role={onProduction ? "alert" : undefined}
                        >
                          {onProduction ? (
                            <>
                              <strong>กำลังจะเขียนลงเซิร์ฟเวอร์ของจริง</strong>
                              <br />
                            </>
                          ) : null}
                          คัดลอกไฟล์รูปขึ้นเครื่องปลายทาง<b>ก่อน</b> แล้วจึงเขียนแถว —
                          ถ้าคัดลอกไม่สำเร็จ จะไม่มีแถวใดถูกเขียนเลย · แถวอยู่ในทรานแซกชันเดียว ·
                          รันซ้ำไม่เพิ่มของซ้ำ
                        </div>

                        <label>พิมพ์ clinic_code ซ้ำเพื่อยืนยัน</label>
                        <input
                          type="text"
                          value={photoConfirm}
                          onChange={(e) => setPhotoConfirm(e.target.value)}
                          placeholder={clinic.trim()}
                        />
                        <div className="row" style={{ marginTop: 10 }}>
                          <button
                            className={`go${onProduction ? " is-prod" : ""}`}
                            type="button"
                            onClick={() => runPhotoImport("apply")}
                            disabled={photoBusy !== "" || photoConfirm.trim() !== clinic.trim()}
                          >
                            {photoBusy === "apply"
                              ? "กำลังนำเข้ารูป…"
                              : `นำเข้ารูป ${photoSurvey.matched} ไฟล์ → ${targetId}`}
                          </button>
                        </div>
                      </>
                    )}

                  {photoDone && <div className="msg ok">{photoDone}</div>}
                  {photoErr && <div className="msg error">{photoErr}</div>}
                  {photoSurvey?.report && (
                    <>
                      <h3>รายงานจาก CLI (รูป)</h3>
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
                        {photoSurvey.report}
                      </pre>
                    </>
                  )}
                  {photoLog && (
                    <>
                      <h3>ผลจากเครื่องปลายทาง</h3>
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
                        {photoLog}
                      </pre>
                    </>
                  )}
            </>
          )}
        </div>
      </div>

  );

  const panelBranches = (
    <div className="panel">
      <h2>2 · สาขาในไฟล์</h2>
      <div className="muted" style={{ fontSize: 12, marginBottom: 8 }}>
        อ่านตาราง <code>branch</code> จากไฟล์ MySQL ที่แนบไว้ · <b>อ่านอย่างเดียว</b> ไม่เขียนอะไรทั้งที่นี่และที่ปลายทาง
        <br />
        ต้องทำขั้นนี้ก่อนสร้างคลินิก เพราะ V4 ออกรหัสสาขาเอง (<code>BR00001</code>…) และเลขสาขาจะติดไปกับ{" "}
        <code>HN</code> ของคนไข้ตลอดชีวิต — รหัสเดิม <code>01</code> เขียนลงไปตรง ๆ ไม่ได้ ต้องแปลง
      </div>

      <div className="row">
        <button
          type="button"
          onClick={uploadAndSurvey}
          disabled={surveying || busy || chosen.length === 0 || !files.sql || tooBig}
        >
          {surveying ? "กำลังอ่าน…" : sessionId ? "อ่านสาขาอีกครั้ง" : "อัปโหลด + อ่านสาขา"}
        </button>
        <span className="muted" style={{ fontSize: 12 }}>
          {chosen.length === 0
            ? "ยังไม่ได้แนบไฟล์ในกล่อง 1"
            : !files.sql
              ? "ยังไม่ได้แนบช่อง MySQL (.sql) — ตารางสาขาอยู่ในไฟล์นี้"
              : sessionId
                ? `ไฟล์อยู่บนเซิร์ฟเวอร์แล้ว ${human(uploadedBytes)} — ขั้นสร้างสคริปต์จะใช้ไฟล์เดิม ไม่ส่งซ้ำ`
                : `ส่งไฟล์ขึ้นเซิร์ฟเวอร์ครั้งเดียว (${human(chosenBytes)}) แล้วอ่านสาขา — ขั้นสร้างสคริปต์จะใช้ไฟล์เดิม`}
        </span>
      </div>

      {surveyErr && <div className="msg error">{surveyErr}</div>}

      {survey && !survey.has_branch_table && (
        <div className="msg info">
          ไฟล์นี้<b>ไม่มีตาราง <code>branch</code></b> — จะนำเข้าเป็นคลินิกสาขาเดียวตามพฤติกรรมเดิม
          <br />
          <span className="muted" style={{ fontSize: 12 }}>
            ถ้าคลินิกนี้มีหลายสาขาจริง แปลว่า dump ไม่ครบ — ต้อง <code>mysqldump</code> ตาราง{" "}
            <code>branch</code> มาด้วย ไม่งั้นคนไข้ทุกคนจะไปอยู่สาขาเดียวกันหมด
          </span>
        </div>
      )}

      {survey && survey.has_branch_table && surveyRows.length > 0 && (
        <>
          <div className="muted" style={{ fontSize: 12, margin: "10px 0 4px" }}>
            อ่านเสร็จใน {surveySeconds}s · พบ {surveyRows.length} สาขา ·{" "}
            {survey.legal_entities?.length ?? 0} นิติบุคคล · คนไข้ {survey.total_patients.toLocaleString()} คน ·
            นัด {survey.total_appointments.toLocaleString()} · บิล {survey.total_bills.toLocaleString()}
          </div>

          <table>
            <thead>
              <tr>
                <th>เอา</th>
                <th>หลัก</th>
                <th>รหัสเดิม</th>
                <th>ชื่อสาขา</th>
                <th>นิติบุคคล</th>
                <th>คนไข้</th>
                <th>นัด</th>
                <th>ห้อง</th>
                <th>บิล</th>
                <th>คอร์ส</th>
                <th>ล็อตสต็อก</th>
              </tr>
            </thead>
            <tbody>
              {surveyRows.map((b) => {
                const on = picked.includes(b.code);
                const empty = branchDataCount(b) === 0;
                return (
                  <tr key={b.code} style={on ? undefined : { opacity: 0.55 }}>
                    <td>
                      <input
                        type="checkbox"
                        aria-label={`เอาสาขา ${b.code} ${b.name}`}
                        checked={on}
                        onChange={(e) =>
                          setPicked((prev) => {
                            const next = e.target.checked
                              ? [...prev, b.code]
                              : prev.filter((c) => c !== b.code);
                            // Unticking the main branch would leave the main-branch choice
                            // pointing at a branch that will not exist, and the main branch
                            // is also where unresolvable rows are filed. Moved, not dropped.
                            if (!e.target.checked && b.code === mainCode) {
                              setMainCode(next[0] ?? "");
                            }
                            if (e.target.checked && !mainCode) setMainCode(b.code);
                            return next;
                          })
                        }
                      />
                    </td>
                    <td>
                      <input
                        type="radio"
                        name="main-branch"
                        aria-label={`ตั้งสาขา ${b.code} เป็นสาขาหลัก`}
                        checked={mainCode === b.code}
                        disabled={!on}
                        onChange={() => setMainCode(b.code)}
                      />
                    </td>
                    <td>
                      <code>{b.code}</code>
                      {!b.active && (
                        <>
                          {" "}
                          <span className="tag">ปิดใช้</span>
                        </>
                      )}
                    </td>
                    <td>
                      {b.name || <span className="muted">(ไม่มีชื่อ)</span>}
                      {empty && (
                        <>
                          {" "}
                          <span className="muted" style={{ fontSize: 11 }}>
                            ไม่มีข้อมูลอ้างถึงเลย
                          </span>
                        </>
                      )}
                    </td>
                    <td style={{ fontSize: 12 }}>
                      {b.legal_name || <span className="muted">(ไม่ระบุ)</span>}
                      {b.tax_id ? (
                        <>
                          <br />
                          <code style={{ fontSize: 11 }}>{b.tax_id}</code>
                        </>
                      ) : null}
                    </td>
                    <td>{b.patients.toLocaleString()}</td>
                    <td>{b.appointments.toLocaleString()}</td>
                    <td>{b.rooms.toLocaleString()}</td>
                    <td>{b.bills.toLocaleString()}</td>
                    <td>{b.courses_bought.toLocaleString()}</td>
                    <td>{b.stock_lots.toLocaleString()}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>

          {/* One TIN per clinic is a database fact, not a preference: clinic_settings holds a
              single tax id. Since 11 Sep 2026 every branch is imported anyway and the header
              carries the majority company's number, so this panel no longer tells the operator
              to split — it names the branches that will print a number which is not theirs, so
              that is a thing somebody accepted rather than a thing nobody was told. */}
          {(() => {
            const other = branchesUnderAnotherTaxID();
            if (other.length === 0) return null;
            const header = majorityTaxID();
            // Grouped by tax id: one line per company, not per branch, because the decision
            // being accepted is about companies.
            const byID = new Map<string, BranchSurveyRow[]>();
            for (const b of other) {
              const id = b.tax_id.trim();
              byID.set(id, [...(byID.get(id) ?? []), b]);
            }
            return (
              <div className="msg warn">
                ใบเสร็จและใบกำกับภาษีของคลินิกนี้จะพิมพ์เลขผู้เสียภาษี <code>{header}</code> ทุกใบ เพราะ V4
                เก็บเลขนี้ได้ <b>ค่าเดียวต่อคลินิก</b> (<code>clinic_settings</code>)
                <br />
                สาขาต่อไปนี้จดทะเบียนเป็นนิติบุคคลอื่น — เอกสารที่ออกที่สาขาเหล่านี้จะ<b>ระบุผู้ขายไม่ตรงกับที่จดทะเบียน</b>
                <ul className="tight">
                  {Array.from(byID.entries()).map(([id, rows]) => (
                    <li key={id}>
                      <code>{id}</code> — {rows[0].legal_name || "(ไม่ระบุชื่อ)"} · สาขา{" "}
                      {rows.map((b) => b.code).join(", ")} ({rows.length} สาขา)
                    </li>
                  ))}
                </ul>
                <span className="muted" style={{ fontSize: 12 }}>
                  ถ้าต้องให้แต่ละนิติบุคคลออกเอกสารในนามของตัวเอง ให้ติ๊กเฉพาะสาขาของเลขเดียวรอบนี้
                  แล้วนำเข้าที่เหลือเป็นคลินิกแยกอีกรอบ
                </span>
              </div>
            );
          })()}

          {strayCodes.length > 0 && (
            <div className="msg warn">
              ในข้อมูลมีรหัสสาขาที่<b>ไม่มีในตาราง branch</b>:{" "}
              {strayCodes.map(([code, n]) => `${code} (${n} แถว)`).join(" · ")} — แถวเหล่านี้จะถูกเขียนไป{" "}
              <b>สาขาหลัก</b> ไม่ถูกทิ้ง เพราะ <code>branch_code</code> เป็น NOT NULL ทุกตาราง
            </div>
          )}

          {survey.cross_branch_hn && survey.cross_branch_hn.length > 0 && (
            <div className="msg warn">
              มี HN ที่โผล่ใต้หลายสาขา {survey.cross_branch_hn.length} ราย — คนไข้คนเดียวจะถูกจัดไปสาขาแรกที่พบ
              <br />
              <span className="muted" style={{ fontSize: 12 }}>
                ตัวอย่าง: {survey.cross_branch_hn.slice(0, 10).join(", ")}
              </span>
            </div>
          )}

          {survey.appointments_no_branch > 0 && (
            <div className="msg info">
              นัด {survey.appointments_no_branch.toLocaleString()} รายการไม่มีสาขาในต้นทาง — ไม่ต้องแก้อะไร
              เพราะ V4 ตั้งสาขาของนัด<b>จากห้องที่จอง</b>เองด้วย trigger{" "}
              <code>validate_appointment_assignment</code>
            </div>
          )}

          <div className={pickedRows.length > 0 && mainRow ? "msg ok" : "msg warn"}>
            {pickedRows.length === 0 ? (
              <>ยังไม่ได้ติ๊กสาขาเลย — ถ้าสร้างคลินิกตอนนี้จะได้สาขาเดียวตามค่าเริ่มต้นของ backend</>
            ) : !mainRow ? (
              <>ยังไม่ได้เลือกสาขาหลัก — ต้องเลือกหนึ่งสาขา เพราะเป็นทั้งสาขาตั้งต้นของเอกสารและปลายทางของแถวที่หาสาขาไม่ได้</>
            ) : (
              <>
                จะสร้าง <b>{pickedRows.length} สาขา</b> · สาขาหลัก:{" "}
                <b>
                  {mainRow.name} (<code>{mainRow.code}</code>)
                </b>
                <br />
                หัวใบเสร็จจะใช้ชื่อนิติบุคคลของสาขาหลัก:{" "}
                <b>{mainRow.legal_name || mainRow.name || "(ไม่ระบุ)"}</b>
                {majorityTaxID() ? (
                  <>
                    {" "}
                    เลขผู้เสียภาษี <code>{majorityTaxID()}</code>
                  </>
                ) : (
                  <>
                    {" "}
                    <span className="muted" style={{ fontSize: 12 }}>
                      · ไม่มีสาขาใดกรอกเลขผู้เสียภาษีไว้ จึงเว้นว่าง ต้องไปกรอกในระบบก่อนออกใบกำกับ
                    </span>
                  </>
                )}
                <span className="muted" style={{ fontSize: 12 }}>
                  {" "}
                  · import จะไม่เขียนทับโปรไฟล์ที่มีคนกรอกไว้แล้ว
                </span>
              </>
            )}
          </div>
        </>
      )}

      {created?.branches && created.branches.length > 0 && (
        <div className="msg ok">
          <b>แผนที่สาขาที่ได้จากปลายทาง</b> — ค่านี้คือสิ่งที่ CLI จะใช้ ไม่ใช่สิ่งที่เราขอไป
          <table>
            <thead>
              <tr>
                <th>รหัสเดิม</th>
                <th>สาขาใน V4</th>
                <th>ชื่อ</th>
                <th>สถานะ</th>
                <th>หลัก</th>
              </tr>
            </thead>
            <tbody>
              {created.branches.map((b) => (
                <tr key={b.branch_code}>
                  <td>
                    <code>{b.source_code || "—"}</code>
                  </td>
                  <td>
                    <code>{b.branch_code}</code>
                  </td>
                  <td>{b.name}</td>
                  <td>{b.status === "created" ? "สร้างใหม่" : "มีอยู่แล้ว"}</td>
                  <td>{b.is_main ? "✓" : ""}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="muted" style={{ fontSize: 12 }}>
            <code>-branch-map {branchMap}</code>
            {mainBranchCode ? (
              <>
                <br />
                <code>-default-branch {mainBranchCode}</code> — แถวที่หาสาขาไม่ได้ไปที่นี่
              </>
            ) : null}
          </div>
          {created.settings ? (
            <div style={{ marginTop: 4 }}>
              หัวใบเสร็จ/ภาษี:{" "}
              <b>
                {created.settings === "written"
                  ? "เขียนให้แล้ว"
                  : created.settings === "kept"
                    ? "มีอยู่แล้ว ไม่แตะของเดิม"
                    : created.settings}
              </b>
            </div>
          ) : null}
        </div>
      )}
    </div>
  );

  const panelGroups = (
    <div className="panel">
        <h2>4 · เลือกสิ่งที่จะนำเข้า</h2>
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
  );

  const panelScript = (
    <div className="panel">
        <h2>5 · สร้างสคริปต์</h2>
        <div className="row">
          <button onClick={submit} disabled={busy || groups.length === 0 || tooBig}>
            {busy ? "กำลังสร้าง…" : "สร้างไฟล์ .sql"}
          </button>
          {chosen.length > 0 && (
            <span className="muted">
              เลือกไว้ {chosen.length} ไฟล์ · รวม {human(chosenBytes)}
            </span>
          )}
        </div>

        {tooBig && (
          <div className="msg error">
            ไฟล์ที่แนบรวมกัน <b>{human(chosenBytes)}</b> เกินเพดาน <b>{human(MAX_UPLOAD_BYTES)}</b> —
            ปุ่มถูกปิดไว้ ยังไม่ได้ส่งอะไรออกไป
          </div>
        )}

        {chosen.length > 0 && !tooBig && (
          <div className="msg info">
            ไฟล์จะถูกหั่นเป็นชิ้นละ {human(CHUNK_BYTES)} แล้วส่งทีละชิ้น (
            {chosen.reduce((n, i) => n + Math.ceil((files[i.field]?.size ?? 0) / CHUNK_BYTES), 0)}{" "}
            ชิ้น) — ไฟล์ใหญ่เท่าไรก็ส่งได้ เพราะแต่ละชิ้นเล็กพอที่ proxy ยอมรับ
            <br />
            เซิร์ฟเวอร์จะ<b>กรองเฉพาะ {MONGO_COLLECTIONS_USED.length} collection ที่ระบบนี้อ่าน</b>{" "}
            ก่อนส่งให้ CLI · ที่เหลือ (log ต่าง ๆ) ถูกทิ้งเหมือนที่ CLI ทิ้งอยู่แล้ว
            และจะมีรายงานว่าเก็บอะไรทิ้งอะไรทุกครั้ง
          </div>
        )}

        {err && <div className="msg error">{err}</div>}

        {result && (
          <>
            <div className="msg info">
              สร้างเสร็จ: <code>{result.filename}</code> · <b>{human(result.sqlBytes)}</b> —{" "}
              <b>อ่านไฟล์ก่อนรัน</b> แล้วรันด้วย
              <br />
              <code>psql &quot;$DATABASE_URL&quot; -v ON_ERROR_STOP=1 -f {result.filename}</code>
              <br />
              <span className="muted" style={{ fontSize: 12 }}>
                ไฟล์อยู่บนเซิร์ฟเวอร์เครื่องมือ ไม่ได้ถูกส่งมาที่เบราว์เซอร์ (สคริปต์ของคลินิกใหญ่
                มีขนาดหลายร้อย MB) · กดดาวน์โหลดแล้วเบราว์เซอร์จะรับเป็นสตรีมตรงลงเครื่อง ·
                เก็บไว้ 6 ชั่วโมงแล้วถูกล้าง
              </span>
            </div>
            <div className="row">
              <button onClick={download}>ดาวน์โหลด {result.filename}</button>
            </div>

            {result.preview && (
              <>
                <h3>หัวไฟล์ (8 KB แรก)</h3>
                <pre
                  style={{
                    background: "var(--panel-2)",
                    border: "1px solid var(--border)",
                    borderRadius: 6,
                    padding: 12,
                    overflowX: "auto",
                    fontSize: 12,
                    whiteSpace: "pre-wrap",
                    maxHeight: 260,
                  }}
                >
                  {result.preview}
                </pre>
              </>
            )}

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
                {/* Whether the photos are coming along is decided by what is staged, so it
                    is stated here rather than left for the operator to infer — the previous
                    version gave no clue and three tenants went in with no images. */}
                <div className="muted" style={{ fontSize: 12, marginTop: 8 }}>
                  {photoSessionId && imgFile ? (
                    <>
                      รอบนี้จะนำเข้า <b>ข้อมูล + รูปหน้าเวชระเบียน</b> ({human(imgFile.size)}) ในครั้งเดียว ·
                      รูปคัดลอกหลัง SQL ผ่านแล้ว ถ้ารูปล้มข้อมูลยังอยู่และกดรูปซ้ำได้
                    </>
                  ) : imgFile ? (
                    <>
                      แนบไฟล์รูปไว้แล้วแต่ยังไม่ได้อัปโหลด — กด{" "}
                      <b>ตรวจก่อน (อ่านอย่างเดียว)</b> ที่ส่วนรูปหนึ่งครั้ง แล้วปุ่มนี้จะนำเข้ารูปให้ด้วย
                    </>
                  ) : (
                    <>
                      รอบนี้จะนำเข้า <b>เฉพาะข้อมูล ไม่มีรูป</b> — ถ้าต้องการรูป แนบ{" "}
                      <code>public_img.tar.gz</code> ที่ส่วน “รูปหน้าเวชระเบียน” ก่อน
                    </>
                  )}
                </div>
                <div className="row" style={{ marginTop: 10 }}>
                  <button
                    className={`go${onProduction ? " is-prod" : ""}`}
                    onClick={importToServer}
                    disabled={importing || !clinic.trim() || serverConfirm.trim() !== clinic.trim()}
                  >
                    {importing
                      ? "กำลังนำเข้า…"
                      : onProduction
                        ? `import to ${selected.label} (ของจริง)${photoSessionId && imgFile ? " + รูป" : ""}`
                        : `import to ${selected.label}${photoSessionId && imgFile ? " + รูป" : ""}`}
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
            {result.log.length > 0 && (
              <>
                <h3>สิ่งที่เซิร์ฟเวอร์ทำก่อนเรียก CLI</h3>
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
                  {result.log.join("\n")}
                </pre>
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
  );

  /* Where the run has actually got to.
   *
   * Derived from the state the form already keeps, NOT from scroll position or from which
   * panel was opened last. Scrolling past a panel is not the same as finishing it, and an
   * indicator that says otherwise is worse than none: it would report progress that does
   * not exist.
   *
   * "current" is the first step that is not done, so a step completed out of order (the
   * target can be picked before the files are attached) does not drag the marker forward
   * past work that still has to happen.
   */
  const steps: { key: string; label: string; done: boolean; node: React.ReactNode }[] = [
    {
      key: "source",
      label: "1 · ที่มาของข้อมูล",
      // Either route counts: files attached here, or dumps the server pulled itself.
      done: chosen.length > 0 || pulledFields.length > 0,
      node: panelSource,
    },
    {
      key: "branches",
      label: "2 · สาขาในไฟล์",
      // A survey alone is not enough — nothing can be imported until the branches to keep
      // are ticked and one of them is named the main branch.
      done: survey !== null && picked.length > 0 && mainCode.trim() !== "",
      node: panelBranches,
    },
    {
      key: "target",
      label: "3 · ปลายทาง และสร้างคลินิก",
      // selected means the environment is configured; clinic + owner are what the CLI
      // refuses to run without.
      done: selected !== null && clinic.trim() !== "" && owner.trim() !== "",
      node: panelTarget,
    },
    {
      key: "groups",
      label: "4 · เลือกสิ่งที่จะนำเข้า",
      done: groups.length > 0,
      node: panelGroups,
    },
    {
      key: "script",
      label: "5 · สร้างสคริปต์",
      // The file existing is the only proof this step finished.
      done: result !== null,
      node: panelScript,
    },
  ];
  const currentIndex = steps.findIndex((s) => !s.done);

  return (
    <div>
      {/* Covers the page while a request is running: the button that started it is at the
          bottom of a long form, where "กำลังสร้าง…" is easy to miss. */}
      <BusyOverlay state={busyState} />

      <div className="flow">
        {steps.map((s, i) => {
          const state = s.done ? "is-done" : i === currentIndex ? "is-current" : "";
          return (
            <Fragment key={s.key}>
              <div className={`flow-rail ${state}${i === steps.length - 1 ? " is-last" : ""}`}>
                <div className="flow-marker">
                  {/* The number and the label repeat the panel's own <h2>, so they are
                      hidden from assistive tech to avoid reading every heading twice. The
                      state is not a repeat of anything, so it is announced. */}
                  <span className="flow-dot" aria-hidden="true">
                    {s.done ? "✓" : i + 1}
                  </span>
                  <span className="flow-label" aria-hidden="true">
                    {s.label}
                  </span>
                  <span className="sr-only">
                    {s.label} ·{" "}
                    {s.done ? "เสร็จแล้ว" : i === currentIndex ? "กำลังทำอยู่" : "ยังไม่ถึง"}
                  </span>
                </div>
              </div>
              <div className="flow-body">{s.node}</div>
            </Fragment>
          );
        })}
      </div>
    </div>
  );
}
