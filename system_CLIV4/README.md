# system_CLIV4

แปลง dump ของระบบเก่า (Dr.Ease V2) เป็นไฟล์ `.sql` ไฟล์เดียวสำหรับ V4 — **ไม่ต่อฐานข้อมูล**

ทำไมออกเป็นไฟล์: อ่านได้ · diff กับรอบก่อนได้ · เก็บเป็นหลักฐานว่ารันอะไรลงไป · และการเอาลงฐานยังเป็นการตัดสินใจของคนที่เห็นของจริงก่อนกด

## ขอบเขต

### MySQL

| ไฟล์ที่ใส่ | ตารางต้นทาง | ปลายทาง V4 |
|---|---|---|
| `-users` | `users` | `users` + `clinic_user` + `clinic_staff` |
| `-doctor` | `doctor` | `doctor` |
| `-room` | **`clinic`** (ของเก่าคือ master ของ **ห้อง**) | `room` |
| `-patient` | `patient` | `patient` |
| `-appointment` | `appointment` | `appointment` |

หรือใช้ **`-sql <ไฟล์เดียว>`** แนบ dump ทั้งฐาน — ระบบหาตารางข้างบนให้เอง และรายงานตารางที่มีในไฟล์
แต่ไม่นำเข้า (เช่น `patient_address`, `patient_diag`, `branch`) ให้เห็นทุกครั้ง

### OPD note (`-only opdnote` · มาจาก `-sql` เท่านั้น)

| ตารางต้นทาง | ปลายทาง V4 |
|---|---|
| `patient_visit_mater` (สะกดตามต้นทาง) | `visit_master` — 1 visit ต่อ visit ของ V2 ตามวันจริง · VN เดิมถ้ายังว่าง ไม่งั้นต่อเลข |
| `patient_opd_note` | `opd_record` (อาการสำคัญ+ประวัติปัจจุบัน → S · ตรวจร่างกาย → O · วินิจฉัย → A · การจัดการ → P) + `opd_visit_advice` (คำแนะนำ) ทุกกล่องมีหัวข้อ |
| `patient_vital_sign` | `vital_sign` (0 = ไม่เคยกรอก · ค่านอกช่วงเครื่องวัดถูกทิ้ง) · อาการเบื้องต้น → ต่อท้ายช่อง S |

กฎทั้งชุดเป็นของรอบ Anjali (C1, 22 ต.ค. 2569) · ทั้งกลุ่มอยู่ในทรานแซกชันเดียว และหยุดเองเมื่อ HN ไม่ใช่คนไข้ของคลินิก
คนไข้ไม่มีสาขา หรือ VN ชน · ไม่เขียนลง visit ที่เซ็นแล้วหรือ visit ที่ไม่ได้เปิดโดยกลุ่มนี้ · รันซ้ำไม่เพิ่มอะไร
**ไม่นำเข้า** กล่อง "การวินิจฉัย" และ "Note เพิ่มเติม" (`patient_diag`) และ `patient_opd_physical` — เหมือนรอบ Anjali

**`patient_address` ไม่นำเข้า** (มติ): ตาราง legacy เก็บจังหวัด/อำเภอ/ตำบลเป็น **ตัวเลข id**
และอัดรหัสไปรษณีย์ไว้ในช่องเดียวกัน (`110302;10540`) ส่วน V4 เก็บเป็น **ชื่อ** — เขียนลงไปตรง ๆ
จะได้ที่อยู่ที่อ่านไม่ออก

### MongoDB (`-mongo` · แคตตาล็อกเท่านั้น)

| collection | ปลายทาง V4 |
|---|---|
| `category` | `catalog_category` |
| `items` | `items` + `catalog_item(item_type='drug')` + `slot_stock` (ล็อตละแถว) |
| `listheals` | `catalog_item(item_type='service')` + `catalog_service` |
| `course_items` | `catalog_item(item_type='course')` + `catalog_course` + `catalog_course_line` |

`catalog_item` คือ identity ที่ตารางอื่นอ้าง จึงเขียนให้ทุกอย่างที่ขายได้ — แถวใน `items`
อย่างเดียวขายไม่ได้ · **ไม่เขียนตาราง `listheal`** ที่ freeze ไว้เป็นหลักฐาน rollback

**ฝั่งเงินยังไม่นำเข้า** — `bills`, `payment_summary`, `course_bills` อยู่ในไฟล์เดียวกัน
แต่ต้องเคาะวิธีลงบัญชีย้อนหลัง (ผ่าน checkout หรือเขียนตารางตรง) และใบกำกับภาษีย้อนหลังก่อน

**ไม่สร้างคลินิก** — ต้องสร้าง `clinic_code` ในระบบก่อน (ผ่าน flow ปกติ เพื่อให้ได้ subscription/settings/rules/counters ครบ) สคริปต์จะตรวจและหยุดถ้าไม่พบ

## ใช้งาน

```powershell
go build -o system_cliv4.exe .

.\system_cliv4.exe -clinic C9D4865 -owner wanaka_admin `
  -users users.sql -doctor doctor.sql -room clinic.sql `
  -patient patient.sql -appointment appointment.sql -address address.ods `
  -out .\out
```

dump ที่รวมทุกตารางไว้ไฟล์เดียวใส่ซ้ำได้ทุกช่อง — ระบบดึงตารางที่ตรงกับช่องนั้นเอง

จากนั้น **อ่านไฟล์ก่อน** แล้วจึงรัน:

```powershell
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f .\out\import_C9D4865.sql
```

## กฎที่ฝังอยู่ในสคริปต์ที่ได้

**ทุกอย่างอยู่ใน 1 transaction** — guard ตัวใดพลาด = rollback ทั้งใบ

| # | Guard / กฎ | ทำอะไร |
|---|---|---|
| 1 | clinic ต้องมีอยู่ | `patient`/`appointment` ไม่มี FK ไป `clinic` — พิมพ์รหัสผิดแล้วแถวจะลอยแบบเงียบ ๆ จึงต้อง `RAISE` ก่อน |
| 2 | `role_presets` ต้องครบ | ไม่ครบแล้ว lookup คืน NULL → error จะไปโผล่เป็น "null value in column role_preset_id" ที่อ่านไม่รู้เรื่อง |
| 3 | users ห้ามชนคลินิกอื่น | `users.id` เป็น PK ทั้งแพลตฟอร์ม · `users.email` UNIQUE ทั้งตาราง → ชนคลินิกอื่น = ปฏิเสธทั้งใบ |
| 4 | insert-only | `ON CONFLICT DO NOTHING` / `NOT EXISTS` — รันซ้ำไม่เพิ่มของซ้ำ |
| 5 | HN ซ้ำในไฟล์ | ปฏิเสธตอน generate (สองเรคคอร์ดอ้างตัวตนเดียวกัน ห้าม merge เอง) |
| 6 | owner | บัญชีที่ `-owner` ระบุได้ `owner` ใน `clinic_user` · ที่เหลือ `counter_staff` · ไม่พบ = ปฏิเสธ |
| 7 | doctor ต้องจองนัดได้ | หมอได้ role `doctor` ใน `clinic_staff` + ผูก `doctor.staff_id` — ไม่งั้น trigger `validate_appointment_assignment` ปฏิเสธนัดทุกใบ |
| 8 | นัดที่หมอหาย | `doctor_code` ที่ V4 ไม่รับ → ล้างเป็น NULL **แต่เก็บรหัสเดิมไว้ที่ `doc.legacy_doctor_code`** พร้อมเหตุผล (ไม่มีข้อมูลหาย) |
| 9 | RLS | `SET LOCAL app.clinic_code` ต้นสคริปต์ — จำเป็นเมื่อรันด้วย role ที่ถูก RLS บังคับ ไม่งั้นเขียนไม่ลงแบบเงียบ |
| 10 | sequence | `setval` ดัน `users_id_seq` ให้พ้น id ที่ preserve ไว้ กันชนกับ user ที่แอปสร้างทีหลัง |

ผลลัพธ์เป็น **deterministic** (ไม่มี timestamp ในหัวไฟล์) — dump ชุดเดิมได้ไฟล์เหมือนกันทุกไบต์ เพื่อให้ diff สองรอบได้

## ตรวจหลัง import

```sql
-- นัดที่ถูกล้าง doctor_code (รหัสเดิมยังอยู่)
SELECT migrate_source_ref, doc->>'legacy_doctor_code'
  FROM appointment WHERE clinic_code = 'C9D4865' AND doc ? 'legacy_doctor_code';

-- หมอที่ยังจองนัดไม่ได้ (staff_id ไม่ผูก)
SELECT doctor_code FROM doctor WHERE clinic_code = 'C9D4865' AND staff_id IS NULL;
```

## หน้าเว็บ

`C:\gin_project\system_importV4` (Next.js) เรียก binary นี้ฝั่ง server แล้วให้ดาวน์โหลด `.sql` — ตั้ง `IMPORT_CLI_PATH` ชี้มาที่ `system_cliv4.exe`

---

## มติที่ใช้ในการ map (เคาะโดยเจ้าของงาน)

| เรื่อง | มติ | อยู่ที่ |
|---|---|---|
| ยา/ของที่ข้อมูลไม่ครบ | ราคา ≤ 0 หรือฉลากยาไม่ครบ 5 ฟิลด์ → `catalog_item.status = 2` (**draft** = รับเข้าคลังได้ ขายไม่ได้) | `catalogStatus` ใน `catalog.go` |
| ชนิดของ `items` | **ทุกแถวเป็น `drug`** | `buildItems` |
| สาขาของล็อต | `slot_stock.branch_code` = `stocks.stock_branch` **ตรง ๆ** (ได้ `01` และ `center`) | `buildItems` |
| รหัสคอร์ส | ต้นทางไม่มีรหัส → mint `CS001..` (ห้ามใช้ `LC` เพราะ `listheals` ใช้อยู่แล้ว 92 รหัส) | `CourseCodePrefix` |
| บริการที่รหัสซ้ำ | ย้ายตัวที่ซ้ำไปรหัสว่างถัดไปของ prefix เดิม + รายงาน (ตาม precedent `LC033 → LC092`) | `RecodeDuplicateServiceCodes` — ตั้ง `false` เพื่อเปลี่ยนเป็นปฏิเสธทั้ง run |
| บัญชีเดียวผูกหลายหมอ | หมอคนที่ 2 ขึ้นไปได้ **ใบพนักงานของตัวเอง** (`has_login=false`, ชื่อจากตาราง doctor) เพื่อให้จองนัดได้ทั้งคู่ | `extraDoctorStaffRows` |
| นัดที่หมอหาย | ล้าง `doctor_code` แต่เก็บรหัสเดิมไว้ที่ `doc.legacy_doctor_code` | `buildAppointment` |
| เงิน | **ยังไม่นำเข้า** | — |

## สิ่งที่ต้องแจ้งคลินิกทุกครั้ง (CLI รายงานให้เอง)

- SKU ที่เป็น draft — ต้องใส่ราคาขายและฉลากยาก่อนขายได้
- บริการที่ถูกย้ายรหัส — รหัสต่างจากระบบเก่า
- ยาในคอร์ส (`list_medicien`) ที่ไม่ถูกนำเข้า — V4 บังคับให้ระบุว่าอยู่ใต้บริการใด ต้นทางไม่ได้บอก
- `kind` ของบริการ (หัตถการ/Lab/อื่นๆ) ปล่อย NULL — V4 ไม่มีแถว `service_kind` ให้อ้าง จึงไม่เดา
