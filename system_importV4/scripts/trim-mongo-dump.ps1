<#
.SYNOPSIS
  ตัด Mongo dump (.js) ให้เหลือเฉพาะ collection ที่ system_CLIV4 อ่าน

.DESCRIPTION
  ทำงานบนเครื่อง Windows กับไฟล์ที่ export มาแล้ว จึงไม่ต้องเข้าเครื่องระบบเก่าอีกรอบ
  (สคริปต์ฝาแฝดที่รันบนเครื่อง V2 คือ export-v2-mongo.sh — ใช้เมื่อยัง export ไม่ได้)

  เหตุที่ต้องตัด: เส้นทางอัปโหลดรับได้ 64 MB (nginx client_max_body_size และ MAX_TOTAL_BYTES
  ของ /api/generate) และ CLI โหลดไฟล์ทั้งก้อนเข้าหน่วยความจำ วัดจริงแล้วกินราว 6.4 เท่าของ
  ขนาดไฟล์ ทุก collection ที่ CLI ไม่รู้จักจึงเป็นน้ำหนักเปล่าทั้งขึ้นและตอนประมวลผล

  อ่านและเขียนแบบสตรีมทีละบรรทัด ไฟล์ระดับกิกะไบต์จึงไม่กินหน่วยความจำ

.PARAMETER Path
  ไฟล์ dump ต้นทาง

.PARAMETER Out
  ไฟล์ผลลัพธ์ (ไม่ระบุ = <ชื่อเดิม>.trimmed.js ข้างไฟล์ต้นทาง)

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File trim-mongo-dump.ps1 -Path "C:\Users\me\Downloads\agafar\mris.js"
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Path,
  [string]$Out
)

$ErrorActionPreference = 'Stop'

# 12 collection ที่ system_CLIV4 อ่านจริง อ่านจากซอร์สไม่ใช่จากความจำ:
#   internal/legacyimport/catalog.go บรรทัด 132-143
#   internal/legacyimport/page.go    บรรทัด 145
$Keep = @(
  'category', 'kind', 'item_types', 'item_units', 'stocks',
  'items', 'listheals', 'course_items',
  'bills', 'payment_summary', 'course_bills',
  'page'
)

if (-not (Test-Path -LiteralPath $Path)) { throw "ไม่พบไฟล์: $Path" }
$src = (Resolve-Path -LiteralPath $Path).Path
if (-not $Out) {
  $Out = Join-Path (Split-Path -Parent $src) ((Split-Path -Leaf $src) -replace '\.js$', '') + '.trimmed.js'
}
if ($Out -eq $src) { throw "ไฟล์ผลลัพธ์ต้องไม่ใช่ไฟล์ต้นทาง" }

$srcBytes = (Get-Item -LiteralPath $src).Length
Write-Host ("ต้นทาง : {0} ({1:N1} MB)" -f $src, ($srcBytes / 1MB))
Write-Host ("ผลลัพธ์ : {0}" -f $Out)
Write-Host ""

# Navicat/mongosh dump เขียน statement ขึ้นต้นบรรทัดด้วย `db.` เสมอ และข้อมูลข้างในย่อหน้า
# ทุกบรรทัด จึงใช้ "บรรทัดที่เริ่มด้วย db. โดยไม่มีช่องว่างนำ" เป็นขอบของ statement ได้
# — วิธีนี้ทนกับ insert ที่กระจายหลายบรรทัด ซึ่งการกรองต่อบรรทัดทำไม่ได้
$reStatement = [regex]'^db\.'
$reInsert = [regex]'^db\.(?:getCollection\("([^"]+)"\)|([A-Za-z0-9_]+))\.(?:insert|insertOne|insertMany|save)\b'

$utf8 = New-Object System.Text.UTF8Encoding($false)   # ไม่ใส่ BOM: parser ฝั่ง Go อ่านไบต์ตรง
$reader = New-Object System.IO.StreamReader($src, [System.Text.Encoding]::UTF8, $true)
$writer = New-Object System.IO.StreamWriter($Out, $false, $utf8)

$keeping = $false
$current = ''
$counts = @{}
$bytesPer = @{}
$dropped = @{}
$lineNo = 0
$sw = [System.Diagnostics.Stopwatch]::StartNew()

try {
  $writer.WriteLine("// trimmed by trim-mongo-dump.ps1 · เก็บเฉพาะ $($Keep.Count) collection ที่ system_CLIV4 อ่าน")
  while ($null -ne ($line = $reader.ReadLine())) {
    $lineNo++

    if ($reStatement.IsMatch($line)) {
      # statement ใหม่: ตัดสินใจใหม่ทุกครั้ง ค่าเดิมไม่รั่วข้าม statement
      $m = $reInsert.Match($line)
      if ($m.Success) {
        $name = if ($m.Groups[1].Success) { $m.Groups[1].Value } else { $m.Groups[2].Value }
        $current = $name
        $keeping = $Keep -contains $name
        if ($keeping) {
          $counts[$name] = 1 + ($counts[$name] | ForEach-Object { $_ })
          if (-not $counts[$name]) { $counts[$name] = 1 }
        }
        else {
          $dropped[$name] = $true
        }
      }
      else {
        # drop() / createCollection() / createIndex() / use — ทิ้งทั้งหมด
        # .drop() ทิ้งเพราะไม่มีเหตุให้พาคำสั่งลบข้ามระบบมา (CLI ข้ามมันอยู่แล้ว
        # แต่ไฟล์ที่ไม่มีมันเลยอ่านแล้วมั่นใจกว่า)
        $keeping = $false
        $current = ''
      }
    }

    if ($keeping) {
      $writer.WriteLine($line)
      if ($current) { $bytesPer[$current] = [int64]$bytesPer[$current] + $line.Length + 1 }
    }
  }
}
finally {
  $writer.Flush(); $writer.Dispose(); $reader.Dispose()
}

$outBytes = (Get-Item -LiteralPath $Out).Length
Write-Host "=== เก็บไว้ ==="
foreach ($k in ($bytesPer.Keys | Sort-Object { - [int64]$bytesPer[$_] })) {
  Write-Host ("  {0,-18} {1,8:N2} MB  ({2} statement)" -f $k, ([int64]$bytesPer[$k] / 1MB), $counts[$k])
}
$missing = $Keep | Where-Object { -not $bytesPer.ContainsKey($_) }
if ($missing) { Write-Host ("  ไม่พบในไฟล์นี้: {0}" -f ($missing -join ', ')) -ForegroundColor Yellow }

Write-Host ""
Write-Host ("=== ทิ้ง {0} collection ===" -f $dropped.Count)
Write-Host ("  {0}" -f (($dropped.Keys | Sort-Object) -join ', '))

Write-Host ""
Write-Host ("อ่าน {0:N0} บรรทัด ใน {1:N1} วินาที" -f $lineNo, $sw.Elapsed.TotalSeconds)
Write-Host ("{0:N1} MB -> {1:N1} MB (เหลือ {2:N1}%)" -f ($srcBytes / 1MB), ($outBytes / 1MB), (100.0 * $outBytes / $srcBytes))

$limit = 64MB
if ($outBytes -gt $limit) {
  Write-Host ""
  Write-Host ("!! ยังเกินเพดาน 64 MB ({0:N1} MB) — นำเข้าแยกสองรอบได้ โดยกล่อง 3 ในหน้าเว็บเลือกเฉพาะบางกลุ่ม" -f ($outBytes / 1MB)) -ForegroundColor Red
  exit 1
}
Write-Host ""
Write-Host ("ผ่านเพดาน 64 MB — แนบไฟล์นี้ในช่อง MongoDB (.js) ได้เลย") -ForegroundColor Green
