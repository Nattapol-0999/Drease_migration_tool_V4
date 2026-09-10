#!/bin/bash
# export-v2-mongo.sh — pull ONLY the collections the importer reads, into one .js file.
#
# WHY THIS EXISTS
# Exporting a whole legacy Mongo database produces a file far larger than the upload path
# accepts (nginx client_max_body_size 64m, and the route buffers the body in ~900 MB of
# RAM). Everything outside the twelve collections below is discarded by the CLI parser
# anyway, so sending it costs upload time and buys nothing.
#
# WHERE TO RUN IT
# On the legacy (V2) server, as a user that can reach mongod. It only reads.
#
#   bash export-v2-mongo.sh                 # search every database
#   bash export-v2-mongo.sh mris dbmongo    # search only these databases (faster)
#   OUT=/tmp/x.js bash export-v2-mongo.sh   # choose the output path
#
# The output is `db.getCollection("<name>").insertMany([...]);` per collection, which is
# one of the shapes internal/mongodump already parses — the same shape the tool's own
# "ดึงจากเซิร์ฟเวอร์" tab produces, so both paths land on tested ground.

set -uo pipefail

OUT="${OUT:-/tmp/v2-import.js}"

# The collections system_CLIV4 actually reads. Verified against the source, not guessed:
#   catalog.go  items · listheals · course_items · category · kind · item_types ·
#               item_units · stocks · bills · payment_summary · course_bills
#   page.go     page
# A name not in this list is skipped by the parser, so exporting it is dead weight.
COLS="category kind item_types item_units stocks items listheals course_items bills payment_summary course_bills page"

# 64 MB is the smallest ceiling on the upload path. The SQL dump shares that budget, so
# warn well before it: ~50 MB of Mongo leaves room for a 10 MB mysqldump.
WARN_BYTES=$((50 * 1024 * 1024))
HARD_BYTES=$((64 * 1024 * 1024))

die() {
  echo "ERROR: $*" >&2
  exit 1
}

# Pick a shell. mongosh is the modern one; `mongo` is what older V2 boxes have.
MONGO_BIN=""
for t in mongosh mongo; do
  if command -v "$t" >/dev/null 2>&1; then
    MONGO_BIN="$t"
    break
  fi
done
[ -n "$MONGO_BIN" ] || die "ไม่พบทั้ง mongosh และ mongo บนเครื่องนี้"

HAVE_EXPORT=0
command -v mongoexport >/dev/null 2>&1 && HAVE_EXPORT=1

echo "shell: $MONGO_BIN · mongoexport: $([ $HAVE_EXPORT -eq 1 ] && echo yes || echo 'no (จะใช้ shell พิมพ์เอง)')"

mongo_eval() { # db  js  → stdout
  "$MONGO_BIN" --quiet "$1" --eval "$2" 2>/dev/null
}

# Which databases to look in. Given as arguments, or every database on the server.
if [ "$#" -gt 0 ]; then
  DBS="$*"
else
  DBS=$(mongo_eval admin 'db.getMongo().getDBNames().join(" ")' | tr -d '\r' | tail -1)
  [ -n "$DBS" ] || die "ลิสต์ฐานข้อมูลไม่ได้ — ระบุชื่อฐานเป็น argument เช่น: bash $0 mris dbmongo"
fi
echo "ฐานที่จะค้น: $DBS"
echo

: > "$OUT" || die "เขียน $OUT ไม่ได้"
printf '// exported by export-v2-mongo.sh on %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "$OUT"

found_any=0
for c in $COLS; do
  # Locate the collection. A count of 0 is treated as absent on purpose: an empty
  # collection contributes nothing and an empty insertMany([]) is noise in the file.
  src=""
  n=0
  for d in $DBS; do
    got=$(mongo_eval "$d" "db.getCollection('$c').count()" | tr -d '\r' | tail -1 | tr -dc '0-9')
    if [ -n "$got" ] && [ "$got" -gt 0 ] 2>/dev/null; then
      src="$d"
      n="$got"
      break
    fi
  done

  if [ -z "$src" ]; then
    printf '  %-16s ไม่พบ / ว่าง — ข้าม\n' "$c"
    continue
  fi

  before=$(wc -c < "$OUT")
  printf 'db.getCollection("%s").insertMany(' "$c" >> "$OUT"
  if [ $HAVE_EXPORT -eq 1 ]; then
    # Canonical Extended JSON, which keeps ObjectId/ISODate/NumberDecimal distinguishable
    # — the parser relies on that to tell an id from a string and money from a float.
    if ! mongoexport --quiet --db "$src" --collection "$c" --jsonArray >> "$OUT"; then
      die "mongoexport $src.$c ล้ม — ไฟล์ $OUT ใช้ไม่ได้ ลบแล้วเริ่มใหม่"
    fi
  else
    # No mongoexport: have the shell print the array itself. EJSON when available
    # (mongosh), tojson otherwise (legacy mongo).
    js="var a=[];db.getCollection('$c').find().forEach(function(d){a.push(typeof EJSON!=='undefined'?EJSON.stringify(d):tojson(d))});print('['+a.join(',')+']')"
    if ! mongo_eval "$src" "$js" >> "$OUT"; then
      die "$MONGO_BIN อ่าน $src.$c ไม่ได้ — ไฟล์ $OUT ใช้ไม่ได้ ลบแล้วเริ่มใหม่"
    fi
  fi
  printf ');\n' >> "$OUT"
  after=$(wc -c < "$OUT")
  printf '  %-16s %8s docs · %6s MB · จาก %s\n' \
    "$c" "$n" "$(awk "BEGIN{printf \"%.1f\", ($after-$before)/1048576}")" "$src"
  found_any=1
done

[ "$found_any" -eq 1 ] || die "ไม่เจอ collection ที่ใช้ได้เลยใน: $DBS"

total=$(wc -c < "$OUT")
mb=$(awk "BEGIN{printf \"%.1f\", $total/1048576}")
echo
echo "เสร็จ: $OUT · ${mb} MB"

if [ "$total" -gt "$HARD_BYTES" ]; then
  echo "!! ใหญ่เกิน 64 MB — อัปโหลดผ่านหน้าเว็บไม่ได้" >&2
  echo "   นำเข้าแยกรอบได้: ลบ bills/payment_summary/course_bills ออกจาก COLS แล้ว export ใหม่" >&2
  echo "   (กล่อง 3 ในหน้าเว็บเลือกเฉพาะบางกลุ่มได้ จึงนำเข้าเป็นสองรอบด้วยไฟล์คนละชุดได้)" >&2
  exit 1
fi
if [ "$total" -gt "$WARN_BYTES" ]; then
  echo "!  เกิน 50 MB — เหลือที่ให้ไฟล์ .sql ไม่ถึง 14 MB (เพดานรวมคือ 64 MB)" >&2
fi
