package legacyimport

// The clinic's LINE OA configuration, read from the V2 mongo export.
//
// # WHY THIS EXISTS
//
// renderLineoaGroup used to write a group with nothing in it but clinic_code and
// group_name, and its own comment admitted the result: "กลุ่มที่ยังไม่มี token และ
// ไม่มีสาขาใน clinic_lineoa_branch ใช้งานจริงไม่ได้อยู่ดี". That is what nine clinics in
// production actually hold — a `<name>_old` group with a zero-length token, no LIFF id
// and no branch mapping — so every one of them had to be finished by hand afterwards.
//
// V2 has the missing values all along. mris.lineChannel carries the LIFF id, the channel
// access token and the branch the OA serves; mris.lineSetting carries the same channel
// without the branch. Reading them turns the empty shell into a group the clinic can use.
//
// # WHICH DOCUMENT WINS
//
// lineChannel, because it is the branch-aware one: it has CHANNEL_NAME and BRANCH, which
// is how the OA is tied to a V4 branch at all. lineSetting is the fallback for a box that
// only has that. Measured on wanaka (10.0.142.133) the two carry the SAME
// LINE_CHANNEL_TOKEN and the same LINE_OA_ID but DIFFERENT LIFF ids
// (lineSetting 2008631059-A0pJabqa vs lineChannel 2008631059-drXwEKvE), so the choice
// is not cosmetic and must be stated rather than left to map iteration order.
//
// # WHAT IS DELIBERATELY NOT CARRIED
//
//	line_oa_id          V4 wants the @basic id ('@280bhwlb'). V2 stores a BOT USER ID
//	                    ('Uf646c81…') or a channel id — neither is an @basic id. Writing
//	                    one would put a wrong value in a field the Customer Portal
//	                    trusts, which is the defect two of the-gloss's groups carry right
//	                    now. It is parsed only so the report can say what was found.
//	portal_liff_id      absent from both documents on every box inspected.
//	line_channel_secret absent as well. Without it the inbound webhook cannot be
//	                    signature-verified, so a clinic imported this way can SEND
//	                    through its OA but not RECEIVE. Said in the report, not guessed.

import (
	"fmt"
	"strings"
)

// lineOAConfig is one V2 OA configuration, after reading and before rendering.
type lineOAConfig struct {
	// source names the collection it came from, for the report.
	source string
	// channelName is lineChannel.CHANNEL_NAME ('' from lineSetting, which has none).
	channelName string
	// v2Branch is lineChannel.BRANCH — the LEGACY branch code, still to be resolved
	// through branchResolver before it can be written.
	v2Branch string
	// liffID is LINE_LIFF_ID, e.g. '2008631059-drXwEKvE'.
	liffID string
	// loginChannelID is the part of liffID before the dash. Derived, never read from the
	// document, because no V2 document carries it as a field of its own.
	loginChannelID string
	// token is LINE_CHANNEL_TOKEN. Written only when the operator passes
	// -line-oa-token; see Input.LineOACarryToken for why that is off by default.
	token string
	// oaID is LINE_OA_ID as V2 holds it. REPORTED ONLY, never written — see the file
	// header.
	oaID string
}

// hasToken reports whether a channel access token was found at all.
func (c lineOAConfig) hasToken() bool { return c.token != "" }

// usable reports whether the configuration carries enough to be worth writing. A LIFF id
// alone is already worth writing: it is what the LIFF entry point needs, and it is not a
// credential.
func (c lineOAConfig) usable() bool { return c.liffID != "" || c.token != "" }

// base64ish reports whether every character could appear in a LINE channel access token.
// Tokens are base64; anything else is either a different kind of value or something that
// would have to be escaped into SQL, and this tool refuses to escape credentials — it
// drops them and says so.
func base64ish(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '+', r == '/', r == '=', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// splitLIFF returns the login channel id encoded in a LIFF id ('<digits>-<suffix>').
// Empty when the shape does not match, so a malformed value is dropped instead of
// producing a half-correct login channel.
func splitLIFF(liff string) (loginChannelID string, ok bool) {
	i := strings.IndexByte(liff, '-')
	if i <= 0 || i == len(liff)-1 {
		return "", false
	}
	head := liff[:i]
	for _, r := range head {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return head, true
}

// readLineOAConfig picks the configuration out of the export.
//
// Returns the zero value when there is nothing usable, which keeps the previous
// behaviour exactly: an empty group the clinic finishes in Settings. Every reason for
// coming back empty is pushed into the report instead of being swallowed.
func readLineOAConfig(mc *mongoCatalog, rep *Report) lineOAConfig {
	docs, source := mc.lineChannel, "mris.lineChannel"
	if len(docs) == 0 {
		docs, source = mc.lineSetting, "mris.lineSetting"
	}
	if len(docs) == 0 {
		rep.notef("LINE OA: dump ไม่มีทั้ง mris.lineChannel และ mris.lineSetting — " +
			"สร้างกลุ่มเปล่าเหมือนเดิม แล้วให้คลินิกกรอก token/LIFF เองที่ Settings → LINE OA")
		return lineOAConfig{}
	}
	// More than one document means more than one OA. This tool writes ONE group per
	// clinic (renderLineoaGroup's guard refuses anything else, because a LINE id is
	// issued by a channel and a binding must name the channel that issued it), so there
	// is no honest way to choose. Nothing is carried, and the operator is told why.
	if len(docs) > 1 {
		rep.warnf("LINE OA: %s มี %d เอกสาร = คลินิกนี้มี OA มากกว่าหนึ่งช่อง · "+
			"เครื่องมือนี้เขียนกลุ่มได้ครั้งละหนึ่งกลุ่ม จึงไม่คัดลอก token/LIFF ให้เลย "+
			"(LINE id ออกโดย channel การเดาว่าชุดไหนมาจากช่องไหนคือการผูกผิดคน) · "+
			"ให้ตั้งค่าแต่ละกลุ่มด้วยมือที่ Settings → LINE OA", source, len(docs))
		return lineOAConfig{}
	}
	d := docs[0]
	cfg := lineOAConfig{
		source:      source,
		channelName: strings.TrimSpace(d.Text("CHANNEL_NAME")),
		v2Branch:    strings.TrimSpace(d.Text("BRANCH")),
		liffID:      strings.TrimSpace(d.Text("LINE_LIFF_ID")),
		token:       strings.TrimSpace(d.Text("LINE_CHANNEL_TOKEN")),
		oaID:        strings.TrimSpace(d.Text("LINE_OA_ID")),
	}
	if cfg.liffID != "" {
		if !base64ish(cfg.liffID) {
			rep.warnf("LINE OA: LINE_LIFF_ID ใน %s มีอักขระที่ไม่ควรมี — ไม่คัดลอกค่านี้", source)
			cfg.liffID = ""
		} else if id, ok := splitLIFF(cfg.liffID); ok {
			cfg.loginChannelID = id
		} else {
			rep.warnf("LINE OA: LINE_LIFF_ID %q ไม่ได้รูป <ตัวเลข>-<ตัวอักษร> — "+
				"คัดลอก LIFF id ไปแต่เว้น line_login_channel_id ไว้", cfg.liffID)
		}
	}
	if cfg.token != "" && !base64ish(cfg.token) {
		rep.warnf("LINE OA: LINE_CHANNEL_TOKEN ใน %s ไม่ใช่ base64 — ไม่คัดลอก token", source)
		cfg.token = ""
	}
	if !cfg.usable() {
		rep.notef("LINE OA: %s ไม่มีทั้ง LINE_LIFF_ID และ LINE_CHANNEL_TOKEN — สร้างกลุ่มเปล่าเหมือนเดิม", source)
		return lineOAConfig{}
	}
	return cfg
}

// reportLineOAConfig says exactly what was found and what will be written, including the
// two things a clinic still has to do by hand. Called once, after the branch is resolved,
// so the message can name the V4 branch rather than the legacy code.
func reportLineOAConfig(cfg lineOAConfig, branch string, carryToken bool, rep *Report) {
	if !cfg.usable() {
		return
	}
	name := cfg.channelName
	if name == "" {
		name = "(ไม่มีชื่อช่องใน V2)"
	}
	rep.notef("LINE OA: อ่านคอนฟิกจาก %s — ช่อง %q · LIFF %s · login channel %s",
		cfg.source, name, orDash(cfg.liffID), orDash(cfg.loginChannelID))
	if branch == "" {
		rep.warnf("LINE OA: %s ไม่ได้ระบุสาขา (BRANCH) — ไม่ผูก clinic_lineoa_branch ให้ "+
			"เพราะการเลือกสาขาแทนต้นทางคือการเดา · กลุ่มที่ยังไม่ผูกสาขาใช้งานจริงไม่ได้ "+
			"และหน้าตั้งค่าจะขึ้นสาขาไว้ใต้ \"สาขาลูกค้าที่ยังไม่กำหนดกลุ่ม\" จนกำหนดเอง", cfg.source)
	} else {
		rep.notef("LINE OA: สาขา V2 %q → %s · ผูกไว้ใน clinic_lineoa_branch แล้ว", cfg.v2Branch, branch)
	}
	switch {
	case !cfg.hasToken():
		rep.warnf("LINE OA: V2 ไม่มี LINE_CHANNEL_TOKEN — OA นี้ยังส่งข้อความไม่ได้จนกรอก token ที่ Settings → LINE OA")
	case !carryToken:
		rep.notef("LINE OA: พบ channel access token (%d ตัวอักษร) แต่ไม่คัดลอกลงไฟล์ "+
			"เพราะไม่ได้ส่ง -line-oa-token · ไฟล์ .sql ที่แจกกันจึงไม่มีความลับอยู่ในตัว · "+
			"วาง token เองที่ Settings → LINE OA แล้วระบบจะเข้ารหัสให้ (sealToken)",
			len(cfg.token))
	default:
		rep.warnf("LINE OA: ไฟล์ .sql นี้มี channel access token อยู่ในตัว (%d ตัวอักษร) — "+
			"ห้าม commit ห้ามส่งต่อ ลบทิ้งเมื่อรันเสร็จ · token ลงฐานเป็น plaintext "+
			"(token_enc_version = 0) เพราะเครื่องมือนี้ไม่มีคีย์ ให้ปิดผนึกต่อด้วย "+
			"`go run ./cmd/lineoaencrypt --apply` แล้วตรวจด้วย `--verify`", len(cfg.token))
	}
	// Both are absent on every V2 box inspected, so this is a statement of fact about the
	// source, not a validation failure.
	rep.notef("LINE OA: V2 ไม่มี @basic id และไม่มี channel secret — line_oa_id กับ " +
		"line_channel_secret จึงเว้นว่าง · ไม่มี secret = รับ webhook เข้าไม่ได้ " +
		"(ส่งออกได้) ต้องไปเอาจาก LINE Developers console")
	if cfg.oaID != "" {
		rep.notef("LINE OA: V2 เก็บ LINE_OA_ID = %s ซึ่งเป็น bot user id ไม่ใช่ @basic id "+
			"จึงไม่เขียนลง line_oa_id (เขียนไปคือใส่ค่าผิดในช่องที่ portal เชื่อ)", cfg.oaID)
	}
}

// lineOADocs counts what the export held, for the survey line in the report.
func lineOADocs(mc *mongoCatalog) string {
	return fmt.Sprintf("lineChannel=%d lineSetting=%d", len(mc.lineChannel), len(mc.lineSetting))
}
