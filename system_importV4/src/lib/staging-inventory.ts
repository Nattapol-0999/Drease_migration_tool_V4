import "server-only";

import { readdir, rm, stat } from "node:fs/promises";
import { join } from "node:path";

import {
  forgetJob,
  jobsDir,
  knownJobs,
  runningJobCount,
  type JobFacts,
} from "./jobs";
import { isValidSessionId, stagedFieldNames, stagingRoot } from "./upload-staging";

/**
 * What is left on this box from imports that did not finish, and how to remove it.
 *
 * # WHY A BUTTON AND NOT JUST THE SWEEPS
 *
 * sweepStaging and sweepJobs already delete anything older than six hours — but they only
 * run when someone STARTS a pull or an upload. An import that fails leaves its dump in
 * place (dropSession is only called on success), and if nobody begins another job the
 * bytes sit there for good. A pulled clinic is 300 MB to 8 GB, so "it will go away on its
 * own eventually" is not true and not cheap.
 *
 * # WHY NOTHING IS DELETED WHILE A JOB RUNS
 *
 * A Job records which directory it writes into, but not which staging session it reads
 * from. So while any job is running there is no way to prove a given session is not the
 * one being read, and deleting the dump out from under a running generate would fail the
 * import for a reason nobody could explain afterwards. Every session is therefore marked
 * busy while runningJobCount() is not zero, and the server refuses those deletions even
 * if a stale browser asks for them. Finished job directories stay removable.
 */

export interface StagedFile {
  name: string;
  bytes: number;
}

export interface SessionEntry {
  id: string;
  files: StagedFile[];
  bytes: number;
  /** epoch ms of the newest thing in the directory */
  modifiedAt: number;
  busy: boolean;
  busyReason?: string;
}

export interface JobEntry {
  id: string;
  kind?: JobFacts["kind"];
  state?: JobFacts["state"];
  clinicCode?: string;
  files: StagedFile[];
  bytes: number;
  modifiedAt: number;
  /** No entry in the in-memory store: left behind by a service restart. */
  orphaned: boolean;
  busy: boolean;
  busyReason?: string;
}

export interface Inventory {
  sessions: SessionEntry[];
  jobs: JobEntry[];
  sessionBytes: number;
  jobBytes: number;
  totalBytes: number;
  runningJobs: number;
  /** The directory being reported on, shown so the operator knows what they are clearing. */
  root: string;
}

export interface ClearRequest {
  sessions: string[];
  jobs: string[];
}

export interface ClearResult {
  removedSessions: number;
  removedJobs: number;
  bytes: number;
  /** Things that were asked for and not removed, with the reason. Never silent. */
  refused: { id: string; kind: "session" | "job"; why: string }[];
}

async function dirEntries(dir: string, names: readonly string[]): Promise<StagedFile[]> {
  const out: StagedFile[] = [];
  for (const name of names) {
    try {
      const s = await stat(join(dir, name));
      if (s.isFile()) out.push({ name, bytes: s.size });
    } catch {
      // not present in this directory
    }
  }
  return out;
}

async function allFiles(dir: string): Promise<StagedFile[]> {
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return [];
  }
  return dirEntries(dir, names);
}

function sum(files: StagedFile[]): number {
  return files.reduce((n, f) => n + f.bytes, 0);
}

async function newestMtime(dir: string): Promise<number> {
  let newest = 0;
  try {
    newest = (await stat(dir)).mtimeMs;
  } catch {
    return 0;
  }
  let names: string[] = [];
  try {
    names = await readdir(dir);
  } catch {
    return newest;
  }
  for (const n of names) {
    try {
      const s = await stat(join(dir, n));
      if (s.mtimeMs > newest) newest = s.mtimeMs;
    } catch {
      // vanished between readdir and stat
    }
  }
  return newest;
}

export async function readInventory(): Promise<Inventory> {
  const root = stagingRoot();
  const running = runningJobCount();
  const busyReason =
    running > 0
      ? `มีงานกำลังทำอยู่ ${running} งาน — ไฟล์ที่ pull ไว้อาจเป็นไฟล์ที่งานนั้นกำลังอ่าน`
      : undefined;

  const sessions: SessionEntry[] = [];
  let names: string[] = [];
  try {
    names = await readdir(root);
  } catch {
    names = [];
  }
  for (const id of names) {
    // Only UUID directories are sessions. `jobs` and anything an operator put here by
    // hand are not touched by this screen.
    if (!isValidSessionId(id)) continue;
    const dir = join(root, id);
    const files = await dirEntries(dir, stagedFieldNames());
    sessions.push({
      id,
      files,
      bytes: sum(files),
      modifiedAt: await newestMtime(dir),
      busy: running > 0,
      busyReason,
    });
  }

  const facts = new Map(knownJobs().map((j) => [j.id, j]));
  const jobs: JobEntry[] = [];
  let jobNames: string[] = [];
  try {
    jobNames = await readdir(jobsDir());
  } catch {
    jobNames = [];
  }
  for (const id of jobNames) {
    if (!isValidSessionId(id)) continue; // job ids are UUIDs too
    const dir = join(jobsDir(), id);
    const files = await allFiles(dir);
    const f = facts.get(id);
    jobs.push({
      id,
      kind: f?.kind,
      state: f?.state,
      clinicCode: f?.clinicCode,
      files,
      bytes: sum(files),
      modifiedAt: await newestMtime(dir),
      orphaned: !f,
      busy: f?.state === "running",
      busyReason: f?.state === "running" ? "งานนี้กำลังทำงานอยู่" : undefined,
    });
  }

  // Newest first: what just failed is what the operator came to clear.
  sessions.sort((a, b) => b.modifiedAt - a.modifiedAt);
  jobs.sort((a, b) => b.modifiedAt - a.modifiedAt);

  const sessionBytes = sessions.reduce((n, s) => n + s.bytes, 0);
  const jobBytes = jobs.reduce((n, j) => n + j.bytes, 0);
  return {
    sessions,
    jobs,
    sessionBytes,
    jobBytes,
    totalBytes: sessionBytes + jobBytes,
    runningJobs: running,
    root,
  };
}

/**
 * clearSelected removes exactly what was asked for, and reports what it would not remove.
 *
 * The inventory is re-read here rather than trusted from the request: the browser's list
 * can be minutes old, and a session that has since become busy must not be deleted
 * because an old screen said it was idle.
 */
export async function clearSelected(req: ClearRequest): Promise<ClearResult> {
  const inv = await readInventory();
  const bySession = new Map(inv.sessions.map((s) => [s.id, s]));
  const byJob = new Map(inv.jobs.map((j) => [j.id, j]));

  const result: ClearResult = {
    removedSessions: 0,
    removedJobs: 0,
    bytes: 0,
    refused: [],
  };

  for (const id of new Set(req.sessions)) {
    if (!isValidSessionId(id)) {
      result.refused.push({ id, kind: "session", why: "รหัสไม่ถูกรูป" });
      continue;
    }
    const s = bySession.get(id);
    if (!s) {
      result.refused.push({ id, kind: "session", why: "ไม่พบแล้ว (อาจถูกล้างไปก่อน)" });
      continue;
    }
    if (s.busy) {
      result.refused.push({ id, kind: "session", why: s.busyReason ?? "กำลังถูกใช้งาน" });
      continue;
    }
    await rm(join(inv.root, id), { recursive: true, force: true });
    result.removedSessions++;
    result.bytes += s.bytes;
  }

  for (const id of new Set(req.jobs)) {
    if (!isValidSessionId(id)) {
      result.refused.push({ id, kind: "job", why: "รหัสไม่ถูกรูป" });
      continue;
    }
    const j = byJob.get(id);
    if (!j) {
      result.refused.push({ id, kind: "job", why: "ไม่พบแล้ว (อาจถูกล้างไปก่อน)" });
      continue;
    }
    if (j.busy) {
      result.refused.push({ id, kind: "job", why: j.busyReason ?? "กำลังทำงานอยู่" });
      continue;
    }
    try {
      await forgetJob(id);
    } catch (err) {
      result.refused.push({
        id,
        kind: "job",
        why: err instanceof Error ? err.message : "ลบไม่สำเร็จ",
      });
      continue;
    }
    result.removedJobs++;
    result.bytes += j.bytes;
  }

  return result;
}
