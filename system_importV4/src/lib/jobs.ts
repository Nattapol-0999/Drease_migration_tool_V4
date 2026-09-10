import "server-only";

import { randomUUID } from "node:crypto";
import { mkdir, readdir, rm, stat } from "node:fs/promises";
import { join } from "node:path";

import { stagingRoot } from "./upload-staging";

/**
 * Long-running work, tracked so the browser can poll instead of holding a request open.
 *
 * WHY THIS EXISTS
 * Cloudflare closes a proxied request when the origin has not answered within 100 seconds
 * and returns 524; that limit is Enterprise-only to change. Generating a script from a
 * real clinic takes far longer than that — measured at 347 seconds for a 897 MB input on
 * this box — so a synchronous endpoint cannot work no matter how the server is tuned.
 * Starting the work and answering with an id keeps every request short.
 *
 * The store is process memory on purpose: this tool runs as a single `next start` process,
 * and a database would add a component to operate for state that is worthless after the
 * import is done. Restarting the service loses in-flight jobs, which is correct — the
 * child process died with it.
 */

export type JobKind = "generate" | "import";
export type JobState = "running" | "done" | "failed";

interface Job {
  id: string;
  kind: JobKind;
  state: JobState;
  /** What is happening right now, in words the operator can read. */
  phase: string;
  log: string[];
  startedAt: number;
  finishedAt?: number;
  user: string;
  clinicCode: string;
  /** Where this job's artifacts live. Removed when the job is swept. */
  dir: string;
  error?: string;

  // generate
  filename?: string;
  sqlPath?: string;
  sqlBytes?: number;
  report?: string;
  /** First lines of the script, so the UI can show something without loading 500 MB. */
  preview?: string;

  // import
  target?: string;
  isProduction?: boolean;
  psqlLog?: string;
}

/** What the browser is allowed to see. Never carries the script itself. */
export interface JobView {
  id: string;
  kind: JobKind;
  state: JobState;
  phase: string;
  log: string[];
  seconds: string;
  clinicCode: string;
  error?: string;
  filename?: string;
  sqlBytes?: number;
  report?: string;
  preview?: string;
  target?: string;
  isProduction?: boolean;
  psqlLog?: string;
}

/** Finished jobs (and their artifacts) are kept this long, then swept. */
const MAX_JOB_AGE_MS = 6 * 60 * 60 * 1000;

// Survives module re-evaluation in dev. In production this is simply a module-level Map.
const store: Map<string, Job> = ((
  globalThis as unknown as { __importv4Jobs?: Map<string, Job> }
).__importv4Jobs ??= new Map<string, Job>());

function jobsRoot(): string {
  return join(stagingRoot(), "jobs");
}

export function jobView(j: Job): JobView {
  const end = j.finishedAt ?? Date.now();
  return {
    id: j.id,
    kind: j.kind,
    state: j.state,
    phase: j.phase,
    log: j.log,
    seconds: ((end - j.startedAt) / 1000).toFixed(1),
    clinicCode: j.clinicCode,
    error: j.error,
    filename: j.filename,
    sqlBytes: j.sqlBytes,
    report: j.report,
    preview: j.preview,
    target: j.target,
    isProduction: j.isProduction,
    psqlLog: j.psqlLog,
  };
}

export function getJob(id: string, user: string): Job | null {
  const j = store.get(id);
  if (!j) return null;
  // A job id is a capability: it is the only thing needed to download a clinic's data, so
  // it is scoped to the operator who started it rather than to anyone holding the string.
  if (j.user !== user) return null;
  return j;
}

/** Handle given to the worker so it can report progress without touching the store. */
export interface JobHandle {
  id: string;
  dir: string;
  setPhase(phase: string): void;
  addLog(...lines: string[]): void;
  /** generate only: records the artifact the job produced. */
  setResult(r: {
    filename: string;
    sqlPath: string;
    sqlBytes: number;
    report: string;
    preview: string;
  }): void;
  /** import only. */
  setImportResult(r: { target: string; isProduction: boolean; psqlLog: string }): void;
}

/**
 * startJob registers the work and returns immediately. `work` runs in the background; its
 * rejection is recorded on the job rather than becoming an unhandled rejection, because a
 * failure the operator cannot see is worse than one that is reported late.
 */
export async function startJob(
  kind: JobKind,
  opts: { user: string; clinicCode: string; phase: string },
  work: (h: JobHandle) => Promise<void>,
): Promise<string> {
  await sweepJobs();

  const id = randomUUID();
  const dir = join(jobsRoot(), id);
  await mkdir(dir, { recursive: true });

  const job: Job = {
    id,
    kind,
    state: "running",
    phase: opts.phase,
    log: [],
    startedAt: Date.now(),
    user: opts.user,
    clinicCode: opts.clinicCode,
    dir,
  };
  store.set(id, job);

  const handle: JobHandle = {
    id,
    dir,
    setPhase: (p) => {
      job.phase = p;
    },
    addLog: (...lines) => {
      job.log.push(...lines);
    },
    setResult: (r) => {
      job.filename = r.filename;
      job.sqlPath = r.sqlPath;
      job.sqlBytes = r.sqlBytes;
      job.report = r.report;
      job.preview = r.preview;
    },
    setImportResult: (r) => {
      job.target = r.target;
      job.isProduction = r.isProduction;
      job.psqlLog = r.psqlLog;
    },
  };

  void work(handle)
    .then(() => {
      job.state = "done";
      job.phase = "เสร็จแล้ว";
      job.finishedAt = Date.now();
    })
    .catch((err: unknown) => {
      job.state = "failed";
      job.phase = "ล้มเหลว";
      job.error = err instanceof Error ? err.message : String(err);
      job.finishedAt = Date.now();
    });

  return id;
}

/** Marks a failure from outside the worker (e.g. a validation done after the job opened). */
export function failJob(id: string, error: string): void {
  const j = store.get(id);
  if (!j) return;
  j.state = "failed";
  j.phase = "ล้มเหลว";
  j.error = error;
  j.finishedAt = Date.now();
}

export async function dropJob(id: string): Promise<void> {
  const j = store.get(id);
  store.delete(id);
  if (j) await rm(j.dir, { recursive: true, force: true });
}

/**
 * sweepJobs removes finished jobs and their artifacts once they are old enough, and
 * deletes job directories with no entry in the store — which is what a service restart
 * leaves behind. A 500 MB script per import would otherwise accumulate silently.
 */
export async function sweepJobs(): Promise<void> {
  const cutoff = Date.now() - MAX_JOB_AGE_MS;

  for (const [id, j] of store) {
    if (j.state === "running") continue;
    if ((j.finishedAt ?? j.startedAt) > cutoff) continue;
    store.delete(id);
    await rm(j.dir, { recursive: true, force: true });
  }

  let dirs: string[];
  try {
    dirs = await readdir(jobsRoot());
  } catch {
    return;
  }
  for (const name of dirs) {
    if (store.has(name)) continue;
    const p = join(jobsRoot(), name);
    try {
      const s = await stat(p);
      // Orphaned by a restart. The grace period avoids racing a job whose directory was
      // created moments ago by another request.
      if (s.mtimeMs < Date.now() - 60_000) await rm(p, { recursive: true, force: true });
    } catch {
      /* already gone */
    }
  }
}
