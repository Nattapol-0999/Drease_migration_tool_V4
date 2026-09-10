# ตัด mysqldump ให้เหลือ INSERT ของตารางที่ระบุ เพื่อทดสอบทีละส่วน
#
# ไม่ใช่ตัว parse SQL: ตัดระดับบรรทัดตามชื่อตารางใน `INSERT INTO` เท่านั้น
# (legacydump.Parse หาแค่ INSERT ไม่ต้องมี CREATE TABLE)
param(
  [Parameter(Mandatory = $true)][string]$Path,
  [Parameter(Mandatory = $true)][string]$Out,
  [Parameter(Mandatory = $true)][string[]]$Tables
)

$ErrorActionPreference = 'Stop'
$want = @{}
foreach ($t in $Tables) { $want[$t] = $true }

$reader = New-Object System.IO.StreamReader($Path, [System.Text.Encoding]::UTF8, $true)
$writer = New-Object System.IO.StreamWriter($Out, $false, (New-Object System.Text.UTF8Encoding($false)))
$keeping = $false
$kept = @{}
$lines = 0

try {
  while ($null -ne ($line = $reader.ReadLine())) {
    $lines++
    if ($line -match '^INSERT INTO `([a-zA-Z0-9_]+)`') {
      $tbl = $matches[1]
      $keeping = $want.ContainsKey($tbl)
      if ($keeping) { $kept[$tbl] = 1 + [int]$kept[$tbl] }
    }
    elseif ($line -match '^(CREATE|DROP|ALTER|LOCK|UNLOCK|--|/\*|SET |COMMIT|/\*!)') {
      # คำสั่งอื่นจบบล็อก INSERT ก่อนหน้า
      $keeping = $false
    }
    if ($keeping) { $writer.WriteLine($line) }
  }
}
finally {
  $writer.Flush(); $writer.Dispose(); $reader.Dispose()
}

$size = (Get-Item -LiteralPath $Out).Length
Write-Output ("อ่าน {0:N0} บรรทัด · เขียน {1:N1} MB" -f $lines, ($size / 1MB))
foreach ($t in ($kept.Keys | Sort-Object)) {
  Write-Output ("  {0,-16} {1} statement" -f $t, $kept[$t])
}
$missing = $Tables | Where-Object { -not $kept.ContainsKey($_) }
if ($missing) { Write-Output ("  ไม่พบในไฟล์: {0}" -f ($missing -join ', ')) }
