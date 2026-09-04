"use client";

import { useState } from "react";

import "./login.css";

// The login form. A client component because it has to show the reason a login failed;
// a plain form POST would either lose the message or need a query-string round trip.

export default function LoginPage() {
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    setError("");
    setBusy(true);
    try {
      const res = await fetch("/api/login", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ user, password }),
      });

      let payload: { ok?: boolean; error?: string } = {};
      try {
        payload = (await res.json()) as typeof payload;
      } catch {
        // A non-JSON body means something in front of the app answered (proxy, gateway).
        // Fall through to the status-based message rather than pretending it parsed.
      }

      if (!res.ok || !payload.ok) {
        setError(payload.error || `เข้าสู่ระบบไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }

      // Read the intended destination from the URL the middleware sent us to. Only
      // same-site paths are honoured, so a crafted ?next= cannot bounce the operator to
      // another host after they authenticate.
      const next = new URLSearchParams(window.location.search).get("next");
      const safeNext = next && next.startsWith("/") && !next.startsWith("//") ? next : "/";
      // replace(), so Back does not land on the login form of a finished session.
      window.location.replace(safeNext);
    } catch (err) {
      setError(err instanceof Error ? `ต่อเซิร์ฟเวอร์ไม่ได้: ${err.message}` : "ต่อเซิร์ฟเวอร์ไม่ได้");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1>system_importV4</h1>
        <p className="login-sub">เครื่องมือย้ายข้อมูล Dr.Ease V2 → V4 · ต้องเข้าสู่ระบบก่อนใช้งาน</p>

        {error ? (
          <p className="login-error" role="alert">
            {error}
          </p>
        ) : null}

        <label className="login-field">
          <span>ชื่อผู้ใช้</span>
          <input
            name="user"
            value={user}
            onChange={(e) => setUser(e.target.value)}
            autoComplete="username"
            autoFocus
            required
          />
        </label>

        <label className="login-field">
          <span>รหัสผ่าน</span>
          <input
            name="password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </label>

        <button className="login-btn" type="submit" disabled={busy}>
          {busy ? "กำลังตรวจสอบ…" : "เข้าสู่ระบบ"}
        </button>
      </form>
    </div>
  );
}
