package legacyimport

import (
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
	"system_cliv4/internal/mongodump"
)

// Branch survey: what an export says about the clinic's branches, before anything is
// imported.
//
// WHY THIS EXISTS
// The importer has always stamped one branch on everything (see config.BranchCode). That
// held for the first tenants — their room master only ever used '01' — but it is not a
// property of V2: the legacy `branch` table is a real registry, `patient.branch_code` is
// NOT NULL, and bills carry their own branch. A tenant with nine branches imported under
// that assumption would have every patient, appointment and bill filed under one of them,
// which is not a smaller mistake than failing outright.
//
// This reads, counts and reports. It maps nothing and writes nothing: how V2 branches
// become V4 clinics is a decision with tax identity in it (branches can belong to
// different legal entities, each with its own TIN), and that is not a decision code gets
// to make. See docs and the operator's answer.

// BranchInfo is one row of the legacy `branch` table plus what the data says about it.
type BranchInfo struct {
	Code string `json:"code"`
	// Name is branch_name — the human name of the branch ("สาขา พระราม 9").
	Name string `json:"name"`
	// LegalName is branch_name_th, which the legacy UI uses for the LEGAL ENTITY on
	// documents, not for the branch. Two branches with different values here are two
	// different companies.
	LegalName string `json:"legal_name"`
	NameEN    string `json:"name_en"`
	TaxID     string `json:"tax_id"`
	LicenseNo string `json:"license_no"`
	Address   string `json:"address"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`

	// ── how much data points at this branch ──
	Patients      int `json:"patients"`
	Appointments  int `json:"appointments"`
	Rooms         int `json:"rooms"`
	Bills         int `json:"bills"`
	Receipts      int `json:"receipts"`
	CoursesBought int `json:"courses_bought"`
	StockLots     int `json:"stock_lots"`
}

// LegalEntity groups branches that share a tax identity. This is the grouping that
// matters for invoicing: a tax invoice must carry the TIN of the entity that issued it.
type LegalEntity struct {
	TaxID    string   `json:"tax_id"`
	Name     string   `json:"name"`
	Branches []string `json:"branches"`
}

// BranchSurvey is the whole answer, ready to print or hand to a UI as JSON.
type BranchSurvey struct {
	Branches []BranchInfo `json:"branches"`
	// Entities is Branches grouped by tax id. More than one means the tenant cannot be
	// one V4 clinic without issuing tax invoices under the wrong company.
	Entities []LegalEntity `json:"legal_entities"`

	// ── things that need a decision or a fix ──
	//
	// UnknownInData are branch codes that appear in the DATA but not in the `branch`
	// table. Rows carrying them would be imported pointing at a branch that does not
	// exist, so they are reported rather than quietly defaulted.
	UnknownInData map[string]int `json:"unknown_in_data,omitempty"`
	// PatientsNoBranch / AppointmentsNoBranch count rows with a blank branch. In V4 the
	// column is NOT NULL (migration 0320), so each of these needs a branch chosen for it.
	PatientsNoBranch     int `json:"patients_no_branch"`
	AppointmentsNoBranch int `json:"appointments_no_branch"`
	// CrossBranchHN lists HNs recorded under more than one branch. Splitting the tenant
	// into several V4 clinics duplicates these patients, and their history splits with
	// them — the number is how much that costs.
	CrossBranchHN []string `json:"cross_branch_hn,omitempty"`
	// DuplicateDocNos are document numbers that appear on more than one bill. The
	// importer refuses the whole script when it sees one (invoice.no is unique per
	// clinic), so this predicts that failure before an import is attempted, and says
	// whether the per-branch number would avoid it.
	DuplicateDocNos     []string `json:"duplicate_doc_nos,omitempty"`
	DuplicateDocNoCount int      `json:"duplicate_doc_no_count"`
	BranchDocNosUnique  bool     `json:"branch_doc_nos_unique"`
	BillsMissingDocNo   int      `json:"bills_missing_doc_no"`
	// MainStockLots counts lots sitting in a stock that NO branch claims through
	// branch.stock_id — V4's central warehouse (MAIN_STOCK). Reported on its own because it
	// is neither a branch's stock nor an error, and because V4 keeps the warehouse out of
	// the "รวมทุกสาขา" total on purpose: filed against a branch instead, these lots would
	// both sit on the wrong shelf and inflate that clinic's inventory value.
	MainStockLots int `json:"main_stock_lots"`
	// LotsWithNoStock counts lots whose document names no stock at all. Nothing says where
	// the box is, so the importer drops them rather than choosing a shelf.
	LotsWithNoStock   int  `json:"lots_with_no_stock"`
	TotalBills        int  `json:"total_bills"`
	TotalPatients     int  `json:"total_patients"`
	TotalAppointments int  `json:"total_appointments"`
	HasBranchTable    bool `json:"has_branch_table"`
}

const maxListed = 25

// SurveyBranches reads every input it was given and reports the branch picture. Missing
// inputs are simply not counted; it never fails for lack of one, because an operator
// surveying a partial export still deserves the part they have.
func SurveyBranches(in Input) (*BranchSurvey, error) {
	s := &BranchSurvey{UnknownInData: map[string]int{}}
	byCode := map[string]*BranchInfo{}
	// stockOwner is branch.stock_id → branch_code: which Mongo stock document belongs to
	// which branch. Built from the same rows as byCode so the registry is read once.
	stockOwner := map[string]string{}

	// ── the registry itself ──
	if len(in.BranchDump) > 0 {
		_, rows, err := legacydump.Parse(string(in.BranchDump), "branch")
		if err != nil {
			return nil, fmt.Errorf("อ่านตาราง branch ไม่ได้: %w", err)
		}
		s.HasBranchTable = len(rows) > 0
		for _, r := range rows {
			code := strings.TrimSpace(r.Get("branch_code"))
			if code == "" {
				continue
			}
			b := &BranchInfo{
				Code:      code,
				Name:      r.Get("branch_name"),
				LegalName: r.Get("branch_name_th"),
				NameEN:    r.Get("branch_name_en"),
				TaxID:     r.Get("branch_tax_identification"),
				LicenseNo: r.Get("branch_license_no"),
				Address:   r.Get("branch_address"),
				Phone:     r.Get("branch_phone"),
				Email:     r.Get("email"),
				Active:    r.Get("active") != "0",
				CreatedAt: r.Get("created_at"),
			}
			byCode[code] = b
			// stock_id names this branch's document in the Mongo `stocks` collection. It is
			// how a lot's location is decided, so it is captured here rather than inferred
			// later from the stocks.stock_branch string.
			if sid := strings.TrimSpace(r.Get("stock_id")); sid != "" {
				stockOwner[sid] = code
			}
		}
	}

	// count records a hit against a known branch, or against UnknownInData when the code
	// is not in the registry. A blank code is the caller's business to count.
	count := func(code string, add func(*BranchInfo)) {
		code = strings.TrimSpace(code)
		if code == "" {
			return
		}
		if b, ok := byCode[code]; ok {
			add(b)
			return
		}
		s.UnknownInData[code]++
	}

	// ── patients: the only table where V2 makes the branch mandatory ──
	if len(in.PatientDump) > 0 {
		_, rows, err := legacydump.Parse(string(in.PatientDump), "patient")
		if err != nil {
			return nil, fmt.Errorf("อ่านตาราง patient ไม่ได้: %w", err)
		}
		s.TotalPatients = len(rows)
		hnBranches := map[string]map[string]bool{}
		for _, r := range rows {
			code := strings.TrimSpace(r.Get("branch_code"))
			if code == "" {
				s.PatientsNoBranch++
			}
			count(code, func(b *BranchInfo) { b.Patients++ })

			if hn := strings.TrimSpace(r.Get("HN")); hn != "" && code != "" {
				if hnBranches[hn] == nil {
					hnBranches[hn] = map[string]bool{}
				}
				hnBranches[hn][code] = true
			}
		}
		for hn, set := range hnBranches {
			if len(set) > 1 {
				s.CrossBranchHN = append(s.CrossBranchHN, hn)
			}
		}
		sort.Strings(s.CrossBranchHN)
	}

	if len(in.AppointmentDump) > 0 {
		_, rows, err := legacydump.Parse(string(in.AppointmentDump), "appointment")
		if err != nil {
			return nil, fmt.Errorf("อ่านตาราง appointment ไม่ได้: %w", err)
		}
		s.TotalAppointments = len(rows)
		for _, r := range rows {
			code := strings.TrimSpace(r.Get("branch_code"))
			if code == "" {
				s.AppointmentsNoBranch++
			}
			count(code, func(b *BranchInfo) { b.Appointments++ })
		}
	}

	// The legacy table named `clinic` is the ROOM master, and it carries a branch.
	if len(in.RoomDump) > 0 {
		_, rows, err := legacydump.Parse(string(in.RoomDump), "clinic")
		if err != nil {
			return nil, fmt.Errorf("อ่านตาราง clinic (ห้อง) ไม่ได้: %w", err)
		}
		for _, r := range rows {
			count(r.Get("branch_code"), func(b *BranchInfo) { b.Rooms++ })
		}
	}

	// ── the money side, which lives in Mongo ──
	if len(in.MongoDump) > 0 {
		cols, err := mongodump.Parse(in.MongoDump)
		if err != nil {
			return nil, fmt.Errorf("อ่าน mongo dump ไม่ได้: %w", err)
		}
		byName := map[string][]mongodump.Doc{}
		for _, c := range cols {
			byName[c.Name] = append(byName[c.Name], c.Docs...)
		}

		seenDoc := map[string]int{}
		seenBranchDoc := map[string]int{}
		for _, d := range byName["bills"] {
			s.TotalBills++
			count(d.Text("branch_code"), func(b *BranchInfo) { b.Bills++ })

			no := strings.TrimSpace(d.Text("invoice_no"))
			bno := strings.TrimSpace(d.Text("invoice_branch_no"))
			if no == "" && bno == "" {
				s.BillsMissingDocNo++
			}
			if no != "" {
				seenDoc[no]++
			}
			// The per-branch number is only unique when read together with its branch,
			// which is how V4 would have to store it if it were used.
			if bno != "" {
				seenBranchDoc[strings.TrimSpace(d.Text("branch_code"))+"/"+bno]++
			}
		}
		for no, n := range seenDoc {
			if n > 1 {
				s.DuplicateDocNoCount++
				if len(s.DuplicateDocNos) < maxListed {
					s.DuplicateDocNos = append(s.DuplicateDocNos, fmt.Sprintf("%s ×%d", no, n))
				}
			}
		}
		sort.Strings(s.DuplicateDocNos)
		s.BranchDocNosUnique = true
		for _, n := range seenBranchDoc {
			if n > 1 {
				s.BranchDocNosUnique = false
				break
			}
		}

		for _, d := range byName["payment_summary"] {
			// Receipts record the branch that took the money, which can differ from the
			// branch that issued the bill.
			code := strings.TrimSpace(d.Text("paid_branch.branch_code"))
			if code == "" {
				code = strings.TrimSpace(d.Text("branch_code"))
			}
			count(code, func(b *BranchInfo) { b.Receipts++ })
		}
		for _, d := range byName["course_bills"] {
			count(d.Text("create_by_branch"), func(b *BranchInfo) { b.CoursesBought++ })
		}
		// Stock lots point at a stock, and the SQL `branch` table says which branch owns
		// which stock through branch.stock_id. That reference decides it — the same rule
		// the importer uses, so the numbers here are the numbers that will land.
		//
		// Reading stocks.stock_branch instead put the central warehouse's lots (566 of
		// 3,350 in the export this was built from) into unknown_in_data as if 'center' were
		// a branch code somebody mistyped. It is not an error; it is the warehouse.
		//
		// Only classified when the registry is present. Without it there is nothing to
		// compare a stock against, and calling every lot central would be a guess dressed
		// as a finding.
		if s.HasBranchTable {
			for _, d := range byName["items"] {
				stockID := docObjectID(d, "stock")
				if stockID == "" {
					s.LotsWithNoStock++
					continue
				}
				if code, owned := stockOwner[stockID]; owned {
					count(code, func(b *BranchInfo) { b.StockLots++ })
					continue
				}
				s.MainStockLots++
			}
		}
	}

	// ── assemble ──
	for _, b := range byCode {
		s.Branches = append(s.Branches, *b)
	}
	sort.Slice(s.Branches, func(i, j int) bool { return s.Branches[i].Code < s.Branches[j].Code })

	entities := map[string]*LegalEntity{}
	for _, b := range s.Branches {
		// Branches with no TIN are grouped under their legal name instead, so they show
		// up as their own group rather than silently joining the first company.
		key := b.TaxID
		if key == "" {
			key = "(ไม่มีเลขผู้เสียภาษี) " + b.LegalName
		}
		e, ok := entities[key]
		if !ok {
			e = &LegalEntity{TaxID: b.TaxID, Name: b.LegalName}
			entities[key] = e
		}
		e.Branches = append(e.Branches, b.Code)
	}
	for _, e := range entities {
		sort.Strings(e.Branches)
		s.Entities = append(s.Entities, *e)
	}
	sort.Slice(s.Entities, func(i, j int) bool {
		if s.Entities[i].TaxID != s.Entities[j].TaxID {
			return s.Entities[i].TaxID > s.Entities[j].TaxID
		}
		return s.Entities[i].Name < s.Entities[j].Name
	})

	return s, nil
}

// Text renders the survey for an operator reading a terminal.
func (s *BranchSurvey) Text() string {
	var b strings.Builder

	if !s.HasBranchTable {
		b.WriteString("ไม่พบตาราง branch ในไฟล์ที่ให้มา — สำรวจได้เฉพาะรหัสสาขาที่ปรากฏในข้อมูล\n\n")
	}

	fmt.Fprintf(&b, "สาขาที่พบ %d รายการ\n", len(s.Branches))
	fmt.Fprintf(&b, "%-5s %-26s %-8s %10s %8s %6s %8s %8s %8s %8s\n",
		"รหัส", "ชื่อสาขา", "ใช้งาน", "เลขภาษี", "คนไข้", "ห้อง", "นัด", "บิล", "ใบเสร็จ", "คอร์ส")
	for _, x := range s.Branches {
		active := "ใช่"
		if !x.Active {
			active = "ปิด"
		}
		tax := x.TaxID
		if tax == "" {
			tax = "-"
		}
		fmt.Fprintf(&b, "%-5s %-26s %-8s %10s %8d %6d %8d %8d %8d %8d\n",
			x.Code, trunc(x.Name, 26), active, tax,
			x.Patients, x.Rooms, x.Appointments, x.Bills, x.Receipts, x.CoursesBought)
	}

	fmt.Fprintf(&b, "\nนิติบุคคลที่พบ %d ราย", len(s.Entities))
	if len(s.Entities) > 1 {
		b.WriteString("  ← มากกว่าหนึ่ง: ใบกำกับภาษีต้องออกในนามที่ถูกต้องของแต่ละสาขา")
	}
	b.WriteString("\n")
	for _, e := range s.Entities {
		tax := e.TaxID
		if tax == "" {
			tax = "(ไม่มีเลขผู้เสียภาษี)"
		}
		fmt.Fprintf(&b, "  %-16s %-42s สาขา: %s\n", tax, trunc(e.Name, 42), strings.Join(e.Branches, ", "))
	}

	b.WriteString("\nสิ่งที่ต้องตัดสินใจหรือแก้\n")
	if len(s.UnknownInData) > 0 {
		codes := make([]string, 0, len(s.UnknownInData))
		for c := range s.UnknownInData {
			codes = append(codes, fmt.Sprintf("%s (%d แถว)", c, s.UnknownInData[c]))
		}
		sort.Strings(codes)
		fmt.Fprintf(&b, "  · รหัสสาขาที่อยู่ในข้อมูลแต่ไม่มีในตาราง branch: %s\n", strings.Join(codes, ", "))
	}
	if s.MainStockLots > 0 || s.LotsWithNoStock > 0 {
		fmt.Fprintf(&b, "  · ล็อตสต็อกในคลังที่ไม่ได้เป็นของสาขาใด (คลังหลัก): %d", s.MainStockLots)
		if s.LotsWithNoStock > 0 {
			fmt.Fprintf(&b, " · ล็อตที่ไม่ระบุคลังเลย: %d (ไม่ถูกนำเข้า)", s.LotsWithNoStock)
		}
		b.WriteString("\n    คลังของแต่ละสาขามาจาก branch.stock_id · คลังที่ไม่มีสาขาใดอ้างถึงคือคลังกลาง ไม่ใช่รหัสสาขาที่พิมพ์ผิด\n")
	}
	fmt.Fprintf(&b, "  · คนไข้ทั้งหมด %d · ไม่มีสาขา %d\n", s.TotalPatients, s.PatientsNoBranch)
	fmt.Fprintf(&b, "  · นัดทั้งหมด %d · ไม่มีสาขา %d", s.TotalAppointments, s.AppointmentsNoBranch)
	if s.TotalAppointments > 0 && s.AppointmentsNoBranch == s.TotalAppointments {
		b.WriteString("  ← ทุกแถวไม่มีสาขา ต้องเลือกสาขาให้")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  · HN ที่อยู่หลายสาขา: %d รายการ", len(s.CrossBranchHN))
	if len(s.CrossBranchHN) > 0 {
		shown := s.CrossBranchHN
		if len(shown) > maxListed {
			shown = shown[:maxListed]
		}
		fmt.Fprintf(&b, " (เช่น %s%s)", strings.Join(shown, ", "),
			map[bool]string{true: " …", false: ""}[len(s.CrossBranchHN) > maxListed])
		b.WriteString("\n    ถ้าแยกเป็นหลายคลินิกใน V4 คนไข้กลุ่มนี้จะถูกแยกออกจากกัน ประวัติจะไม่ต่อกัน")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  · บิลทั้งหมด %d · ไม่มีเลขเอกสารเลย %d\n", s.TotalBills, s.BillsMissingDocNo)
	fmt.Fprintf(&b, "  · เลขเอกสารระดับคลินิก (invoice_no) ซ้ำ: %d เลข", s.DuplicateDocNoCount)
	if s.DuplicateDocNoCount > 0 {
		fmt.Fprintf(&b, " (เช่น %s)\n", strings.Join(s.DuplicateDocNos, ", "))
		b.WriteString("    ตัวสร้างสคริปต์จะปฏิเสธทั้งไฟล์เพราะ invoice.no ต้องไม่ซ้ำต่อคลินิก\n")
		if s.BranchDocNosUnique {
			b.WriteString("    แต่เลขรายสาขา (invoice_branch_no) ไม่ซ้ำเมื่อดูคู่กับสาขา — ทางแก้คือแยกคลินิกตามสาขา หรือใช้เลขรายสาขา\n")
		} else {
			b.WriteString("    เลขรายสาขา (invoice_branch_no) ก็ซ้ำ — ต้องเคาะกฎการตั้งเลขก่อนนำเข้า\n")
		}
	} else {
		b.WriteString(" — ไม่ซ้ำ\n")
	}

	return b.String()
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
