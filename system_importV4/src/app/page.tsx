import GenerateForm from "@/components/GenerateForm";
import LogoutButton from "@/components/LogoutButton";

import "./page-header.css";

export default function Home() {
  return (
    <main>
      <div className="page-head">
        <h1>system_importV4</h1>
        <LogoutButton />
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
