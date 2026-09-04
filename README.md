# migration_tool_V4

เครื่องมือย้ายข้อมูลคลินิกจาก **Dr.Ease V2 → V4** สองส่วนที่ทำงานคู่กัน จึงอยู่ใน branch เดียวกัน

| โฟลเดอร์ | คืออะไร |
| --- | --- |
| `system_CLIV4/` | Go CLI · อ่าน dump ของ V2 (MySQL `.sql` + MongoDB `.js`) แล้ว **สร้างไฟล์ `.sql` เดียวที่อ่านตรวจได้** สำหรับ V4 · ไม่ต่อฐานข้อมูลเอง |
| `system_importV4/` | หน้าเว็บ (Next.js 14) ครอบ CLI ตัวนั้น · อัปโหลด dump → กดสร้าง → ดาวน์โหลด `.sql` → หรือกดนำเข้าเข้าฐานปลายทาง |

ทั้งสองต้องเป็นรุ่นที่เข้ากัน เพราะหน้าเว็บเรียกไบนารีของ CLI โดยตรงผ่าน `IMPORT_CLI_PATH`
ถ้าแยกที่เก็บกัน เวอร์ชันจะหลุดกันโดยไม่มีใครรู้

## ทำไมมันปลอดภัยกว่าการรัน SQL มือเปล่า

- CLI **ไม่เขียนฐานข้อมูล** มันสร้างไฟล์เดียวให้อ่านก่อน
- ไฟล์ที่ได้ห่อด้วยทรานแซกชันเดียว มีด่านตรวจ (`RAISE`) เรื่องยอดเงินและความครบถ้วนอยู่ข้างใน
  ผิดที่ไหนก็ rollback ทั้งใบ ไม่มีสภาพครึ่ง ๆ กลาง ๆ
- ปุ่มนำเข้าบนหน้าเว็บบังคับ **พิมพ์ `clinic_code` ยืนยัน** และตรวจว่าไฟล์นั้นเป็นของคลินิกนั้นจริง
- หน้าเว็บทั้งหมดอยู่หลังล็อกอิน (`src/middleware.ts` ปิดทุก path รวม `/api/*` และ fail closed
  ถ้าไม่ได้ตั้ง `AUTH_SECRET`)

## สิ่งที่ห้ามเข้ามาใน repo นี้

- `system_CLIV4/out/` — ไฟล์ `.sql` ที่สร้างออกมา **มีชื่อคนไข้ HN เลขที่บิลและยอดเงินจริง**
  เป็นหลักฐานของการรันหนึ่งครั้ง ควรอยู่ในบันทึกของผู้ปฏิบัติงาน ไม่ใช่ใน version control
- dump ของคลินิก (`*.sql` `*.js` `*.ods`)
- `.env.local` — มีรหัสฐานข้อมูล คีย์เซ็น session และ hash รหัสผ่าน

`.gitignore` กันไว้ทั้งระดับรากและในแต่ละโปรเจกต์แล้ว

## เริ่มใช้

```bash
# CLI
cd system_CLIV4
go build -o system_cliv4.exe .
./system_cliv4.exe -clinic <CODE> -owner <legacy users.code> \
  -sql <dump.sql> -mongo <mris.js> -out ./out

# หน้าเว็บ
cd system_importV4
cp .env.example .env.local     # แล้วเติมค่าให้ครบ
node scripts/hash-password.mjs '<password>'   # ได้ AUTH_PASSWORD_HASH + AUTH_SECRET
npm ci
npm run dev                    # http://localhost:3100
```

`-only` เลือกได้เป็นกลุ่ม: `people` `rooms` `patients` `addresses` `appointments`
`catalog` `stock` `money` `entitlements`
