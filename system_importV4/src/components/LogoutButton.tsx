"use client";

import { useState } from "react";

/**
 * Sign-out control. Reloads to "/" afterwards so the middleware is the thing that
 * decides where an operator without a session belongs — this component does not need to
 * know that the answer is the login page.
 */
export default function LogoutButton() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function logout() {
    setError("");
    setBusy(true);
    try {
      const res = await fetch("/api/logout", { method: "POST" });
      if (!res.ok) {
        setError(`ออกจากระบบไม่สำเร็จ (HTTP ${res.status})`);
        return;
      }
      window.location.replace("/");
    } catch (err) {
      setError(err instanceof Error ? err.message : "ออกจากระบบไม่สำเร็จ");
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <button type="button" className="logout-btn" onClick={logout} disabled={busy}>
        {busy ? "กำลังออก…" : "ออกจากระบบ"}
      </button>
      {error ? (
        <span className="logout-error" role="alert">
          {error}
        </span>
      ) : null}
    </>
  );
}
