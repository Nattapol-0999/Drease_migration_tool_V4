import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "system_importV4",
  description: "นำเข้าข้อมูลคลินิกจากไฟล์ dump เข้า V4 (inspect → map → plan → run)",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="th">
      <body>{children}</body>
    </html>
  );
}
