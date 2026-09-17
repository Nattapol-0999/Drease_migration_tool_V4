import GenerateForm from "@/components/GenerateForm";
import Logo from "@/components/Logo";
import LogoutButton from "@/components/LogoutButton";
import ThemeToggle from "@/components/ThemeToggle";

import "./page-header.css";

export default function Home() {
  return (
    <main>
      <div className="page-head">
        <h1>
          <Logo />
        </h1>
        {/* Grouped so the header stays two columns: the title on the left and the
            controls together on the right, rather than three items spread apart. */}
        <div className="page-head-actions">
          <ThemeToggle />
          <LogoutButton />
        </div>
      </div>
      <p className="sub">
        แปลง dump ของระบบเก่า (Dr.Ease V2) เป็นไฟล์ <code>.sql</code> สำหรับ V4 —
        ไม่ต่อฐานข้อมูล ไม่เขียนอะไรเอง. อ่านไฟล์ที่ได้ก่อน แล้วจึงรันด้วย{" "}
        <code>psql</code> เมื่อพร้อม
      </p>
      <GenerateForm />
    </main>
  );
}
