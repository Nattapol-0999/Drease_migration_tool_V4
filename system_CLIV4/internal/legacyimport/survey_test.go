package legacyimport

import (
	"strings"
	"testing"
)

// Surveying a multi-branch export.
//
// SurveyBranches produces the number every later decision rests on: how many branches the
// operator is shown, which ones get ticked, how many V4 branches get created, and therefore
// what -branch-map contains. Nothing downstream can detect an error here. A branch missed at
// this step is a branch that never gets created, and every row belonging to it is then filed
// at the default — accepted by the database, indistinguishable from real data.
//
// The fixtures below are the shape of the real export that forced this work: nine branches
// across three companies, two of them with no TIN recorded, plus the three damaged codes that
// appear in the data but not in the registry.

// branchRegistry is the legacy `branch` table. Column names are the real ones, including the
// two that do not read like what they hold: branch_name_th is the LEGAL ENTITY the documents
// print, and branch_tax_identification is that entity's TIN.
func branchRegistry() []byte {
	return []byte("INSERT INTO `branch` (`branch_code`, `branch_name`, `branch_name_th`, " +
		"`branch_name_en`, `branch_tax_identification`, `branch_address`, `branch_phone`, " +
		"`email`, `active`) VALUES\n" +
		"('00', 'แผนกการตลาด', '', '', '', '', '', '', 1),\n" +
		"('01', 'สาขา พระราม 9', 'บริษัท แอนจาลี จำกัด', 'Rama 9', '0105558054530', '364 อาคารถาวรเพลส', '020965554', 'r9@x.co', 1),\n" +
		"('02', 'สาขา เกษตรนวมินทร์', 'บจก. แอนจาลี', '', '0105558054530', '', '', '', 1),\n" +
		"('03', 'สาขา ราชพฤกษ์', 'บริษัท เอเจสองสี่ จำกัด', '', '0105566027191', '', '', '', 1),\n" +
		"('04', 'สาขา ชิดลม', 'บริษัท แอนจาลี จำกัด (สำนักงานใหญ่)', '', '0105558054530', '', '', '', 1),\n" +
		"('05', 'ร้านยา', '', '', '', '', '', '', 1),\n" +
		"('06', 'สาขา รังสิต', 'บริษัท เอเจห้าหก จำกัด', '', '0135568021863', '', '', '', 1),\n" +
		"('07', 'แผนกบริการออนไลน์', '', '', '', '', '', '', 0),\n" +
		"('08', 'สาขา บางนา', '', '', '', '', '', '', 1);\n")
}

func TestSurveyReadsEveryBranchInTheRegistry(t *testing.T) {
	s, err := SurveyBranches(Input{BranchDump: branchRegistry()})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if !s.HasBranchTable {
		t.Error("has_branch_table = false with a registry present — the UI uses this flag to decide whether to show the branch step at all")
	}
	if len(s.Branches) != 9 {
		t.Fatalf("branches = %d, want 9", len(s.Branches))
	}
	// Sorted by code, so the table an operator ticks is in the same order every run. An
	// unstable order means the row under the cursor is not the row that was read.
	want := []string{"00", "01", "02", "03", "04", "05", "06", "07", "08"}
	for i, b := range s.Branches {
		if b.Code != want[i] {
			t.Errorf("branches[%d] = %q, want %q — the list must be sorted by code", i, b.Code, want[i])
		}
	}
}

func TestSurveyKeepsTheLegalNameApartFromTheBranchName(t *testing.T) {
	// branch_name is what staff call the place; branch_name_th is the company that issues
	// its receipts. Reading either one for the other puts the wrong name on a tax invoice.
	s, err := SurveyBranches(Input{BranchDump: branchRegistry()})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	byCode := map[string]BranchInfo{}
	for _, b := range s.Branches {
		byCode[b.Code] = b
	}
	if got := byCode["01"].Name; got != "สาขา พระราม 9" {
		t.Errorf("branch 01 name = %q, want the branch name", got)
	}
	if got := byCode["01"].LegalName; got != "บริษัท แอนจาลี จำกัด" {
		t.Errorf("branch 01 legal_name = %q, want the company from branch_name_th", got)
	}
	if got := byCode["01"].TaxID; got != "0105558054530" {
		t.Errorf("branch 01 tax_id = %q", got)
	}
	// active=0 has to survive as false: a closed branch still owns its history, so it is
	// shown rather than dropped, and the operator decides.
	if byCode["07"].Active {
		t.Error("branch 07 has active=0 and must be reported inactive, not filtered out")
	}
	if !byCode["08"].Active {
		t.Error("branch 08 has active=1 and must be reported active")
	}
}

func TestSurveyGroupsBranchesByTaxIDNotByCompanyName(t *testing.T) {
	// This grouping is the whole reason the survey exists rather than a plain count. Three
	// branches carry the same TIN under three different spellings of the company name
	// ("บริษัท แอนจาลี จำกัด", "บจก. แอนจาลี", "...(สำนักงานใหญ่)"). Grouping by name would
	// split one company into three; grouping by TIN keeps them together.
	s, err := SurveyBranches(Input{BranchDump: branchRegistry()})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}

	byTax := map[string][]string{}
	for _, e := range s.Entities {
		byTax[e.TaxID] = e.Branches
	}
	main := byTax["0105558054530"]
	if strings.Join(main, ",") != "01,02,04" {
		t.Errorf("the main company holds %v, want 01,02,04 grouped despite three spellings of its name", main)
	}
	if got := byTax["0105566027191"]; strings.Join(got, ",") != "03" {
		t.Errorf("tax 0105566027191 holds %v, want just 03", got)
	}
	if got := byTax["0135568021863"]; strings.Join(got, ",") != "06" {
		t.Errorf("tax 0135568021863 holds %v, want just 06", got)
	}

	// More than one entity is the signal that this tenant cannot be one V4 clinic without
	// printing the wrong company on somebody's receipt.
	if len(s.Entities) < 2 {
		t.Fatalf("legal_entities = %d, want more than one so the operator is warned to split the import",
			len(s.Entities))
	}
}

func TestSurveyDoesNotMergeBranchesThatRecordedNoTaxID(t *testing.T) {
	// A blank TIN means nobody filled it in, not that the branch belongs to the first
	// company in the file. Merging them would hide four branches inside another entity's
	// group and make a split look unnecessary.
	s, err := SurveyBranches(Input{BranchDump: branchRegistry()})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	for _, e := range s.Entities {
		if e.TaxID != "" {
			continue
		}
		for _, code := range e.Branches {
			if code == "01" || code == "02" || code == "04" {
				t.Errorf("branch %s has a TIN but was grouped with the blank-TIN branches", code)
			}
		}
	}
	blank := 0
	for _, b := range s.Branches {
		if b.TaxID == "" {
			blank++
		}
	}
	if blank != 4 {
		t.Errorf("branches with no TIN = %d, want 4 (00, 05, 07, 08)", blank)
	}
}

func TestSurveyCountsThePatientsEachBranchWouldTakeWithIt(t *testing.T) {
	// The counts are what tell an operator a branch is real. A branch with no rows behind
	// it is a department somebody typed once; a branch with 7,144 patients is the one that
	// must not be left out.
	patients := []byte("INSERT INTO `patient` (`HN`, `branch_code`, `name`) VALUES\n" +
		"('0169001', '04', 'ก'),\n" +
		"('0169002', '04', 'ข'),\n" +
		"('0169003', '01', 'ค'),\n" +
		"('0169004', '', 'ง'),\n" +
		"('0169005', '09', 'จ');\n")

	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), PatientDump: patients})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	byCode := map[string]BranchInfo{}
	for _, b := range s.Branches {
		byCode[b.Code] = b
	}
	if byCode["04"].Patients != 2 {
		t.Errorf("branch 04 patients = %d, want 2", byCode["04"].Patients)
	}
	if byCode["01"].Patients != 1 {
		t.Errorf("branch 01 patients = %d, want 1", byCode["01"].Patients)
	}
	if byCode["08"].Patients != 0 {
		t.Errorf("branch 08 patients = %d, want 0", byCode["08"].Patients)
	}
	if s.TotalPatients != 5 {
		t.Errorf("total_patients = %d, want 5 — every row counted, including the ones with no usable branch",
			s.TotalPatients)
	}
	if s.PatientsNoBranch != 1 {
		t.Errorf("patients_no_branch = %d, want 1", s.PatientsNoBranch)
	}
}

func TestSurveyReportsCodesThatAreInTheDataButNotTheRegistry(t *testing.T) {
	// "09", "68" and "al" are real: three rows of data-entry damage in the export. They
	// cannot be created as branches and they cannot be dropped, so the only honest
	// handling is to count them and say so before anything is imported.
	patients := []byte("INSERT INTO `patient` (`HN`, `branch_code`) VALUES\n" +
		"('0169001', '09'),\n('0169002', '68'),\n('0169003', 'al'),\n('0169004', '09'),\n" +
		"('0169005', '04');\n")

	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), PatientDump: patients})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if s.UnknownInData["09"] != 2 || s.UnknownInData["68"] != 1 || s.UnknownInData["al"] != 1 {
		t.Errorf("unknown_in_data = %v, want 09:2 68:1 al:1", s.UnknownInData)
	}
	// A stray code must never become a tenth branch.
	if len(s.Branches) != 9 {
		t.Errorf("branches = %d, want 9 — a code found only in the data is not a branch", len(s.Branches))
	}
	if !strings.Contains(s.Text(), "09") {
		t.Error("the printed survey does not mention the stray codes; an operator cannot act on what is not said")
	}
}

func TestSurveyFindsPatientsRecordedUnderMoreThanOneBranch(t *testing.T) {
	// This is the cost of splitting a tenant into several V4 clinics: an HN present at two
	// branches becomes two patients whose history no longer joins up. The number has to be
	// on the screen BEFORE the split is chosen, not discovered afterwards.
	patients := []byte("INSERT INTO `patient` (`HN`, `branch_code`) VALUES\n" +
		"('0169001', '01'),\n('0169001', '04'),\n" +
		"('0169002', '02'),\n('0169002', '02'),\n" +
		"('0169003', '06');\n")

	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), PatientDump: patients})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if len(s.CrossBranchHN) != 1 || s.CrossBranchHN[0] != "0169001" {
		t.Errorf("cross_branch_hn = %v, want exactly [0169001] — the same branch twice is not a cross-branch patient",
			s.CrossBranchHN)
	}
}

func TestSurveyCountsRoomsFromTheLegacyClinicTable(t *testing.T) {
	// The legacy table called `clinic` is the ROOM master. Rooms matter to the branch
	// decision because V4 derives an appointment's branch from its room, so a branch with
	// rooms and no patients is still a branch that has to exist.
	rooms := []byte("INSERT INTO `clinic` (`clinic_code`, `clinic_name`, `branch_code`) VALUES\n" +
		"('R1', 'ห้องตรวจ 1', '02'),\n" +
		"('R2', 'ห้องตรวจ 2', '02'),\n" +
		"('R3', 'ห้องหัตถการ', '08');\n")

	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), RoomDump: rooms})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	byCode := map[string]BranchInfo{}
	for _, b := range s.Branches {
		byCode[b.Code] = b
	}
	if byCode["02"].Rooms != 2 {
		t.Errorf("branch 02 rooms = %d, want 2", byCode["02"].Rooms)
	}
	if byCode["08"].Rooms != 1 {
		t.Errorf("branch 08 rooms = %d, want 1 — a branch with rooms and no patients still has to be created",
			byCode["08"].Rooms)
	}
}

func TestSurveyCountsAppointmentsWithNoBranchAtAll(t *testing.T) {
	// Every one of the 20,972 appointment rows in the real export has a blank branch_code.
	// When that ratio is total it is not a data gap to patch quietly — it means the branch
	// has to come from somewhere else entirely, so the survey states it plainly.
	appts := []byte("INSERT INTO `appointment` (`id`, `HN`, `branch_code`) VALUES\n" +
		"(1, '0169001', ''),\n(2, '0169002', ''),\n(3, '0169003', '');\n")

	s, err := SurveyBranches(Input{BranchDump: branchRegistry(), AppointmentDump: appts})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if s.TotalAppointments != 3 || s.AppointmentsNoBranch != 3 {
		t.Fatalf("appointments total=%d no_branch=%d, want 3 and 3",
			s.TotalAppointments, s.AppointmentsNoBranch)
	}
	if !strings.Contains(s.Text(), "ทุกแถวไม่มีสาขา") {
		t.Error("when every appointment lacks a branch the survey must say so outright")
	}
}

func TestSurveyWithNoRegistrySaysSoInsteadOfReportingZeroBranches(t *testing.T) {
	// An export whose `branch` table was not included is not a single-branch clinic. The
	// two have to look different, because treating the first as the second is how a
	// nine-branch tenant gets imported into one branch with nothing to show it happened.
	patients := []byte("INSERT INTO `patient` (`HN`, `branch_code`) VALUES\n" +
		"('0169001', '01'),\n('0169002', '04');\n")

	s, err := SurveyBranches(Input{PatientDump: patients})
	if err != nil {
		t.Fatalf("survey without a registry must still report what it can: %v", err)
	}
	if s.HasBranchTable {
		t.Error("has_branch_table = true with no registry supplied")
	}
	if len(s.Branches) != 0 {
		t.Errorf("branches = %d, want 0 — codes seen only in data are not registry entries", len(s.Branches))
	}
	// The codes still have to surface, as unknowns, so the operator knows the file is
	// incomplete rather than the clinic being small.
	if s.UnknownInData["01"] != 1 || s.UnknownInData["04"] != 1 {
		t.Errorf("unknown_in_data = %v, want 01:1 04:1", s.UnknownInData)
	}
	if !strings.Contains(s.Text(), "ไม่พบตาราง branch") {
		t.Error("the survey must lead with the fact that the registry is missing")
	}
}

func TestSurveyIsReadOnlyAndSurvivesAnEmptyInput(t *testing.T) {
	// The route that calls this runs it inline on an operator's click, before anything has
	// been created. It has to answer rather than fail on a partial export.
	s, err := SurveyBranches(Input{})
	if err != nil {
		t.Fatalf("empty input must not be an error: %v", err)
	}
	if s.HasBranchTable || len(s.Branches) != 0 || len(s.Entities) != 0 {
		t.Errorf("empty survey reported content: table=%v branches=%d entities=%d",
			s.HasBranchTable, len(s.Branches), len(s.Entities))
	}
	if s.TotalPatients != 0 || s.PatientsNoBranch != 0 {
		t.Errorf("empty survey counted patients: total=%d blank=%d", s.TotalPatients, s.PatientsNoBranch)
	}
}

func TestSurveyIsDeterministic(t *testing.T) {
	// Two runs over one file must produce the same list in the same order. The branch table
	// is assembled from a map, and Go randomises map iteration — without the sorts, the
	// order an operator ticks would differ from the order that was read.
	in := Input{BranchDump: branchRegistry()}
	first, err := SurveyBranches(in)
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	for i := 0; i < 12; i++ {
		next, err := SurveyBranches(in)
		if err != nil {
			t.Fatalf("survey run %d: %v", i, err)
		}
		for j := range first.Branches {
			if first.Branches[j].Code != next.Branches[j].Code {
				t.Fatalf("run %d branch %d = %q, first run had %q",
					i, j, next.Branches[j].Code, first.Branches[j].Code)
			}
		}
		if len(first.Entities) != len(next.Entities) {
			t.Fatalf("run %d has %d entities, first run had %d",
				i, len(next.Entities), len(first.Entities))
		}
		for j := range first.Entities {
			if first.Entities[j].TaxID != next.Entities[j].TaxID {
				t.Fatalf("run %d entity %d = %q, first run had %q",
					i, j, next.Entities[j].TaxID, first.Entities[j].TaxID)
			}
		}
	}
}

// ── where a stock lot lives ───────────────────────────────────────────────────────────
//
// The SQL `branch` table names each branch's stock through branch.stock_id, which holds the
// Mongo _id of that branch's document in the `stocks` collection. That reference decides a
// lot's location:
//
//	lot.stock IS in branch.stock_id  → that branch's shelf
//	lot.stock is NOT in it           → no branch claims it = the central warehouse
//
// The earlier rule read the stocks.stock_branch string instead. Both agree on the real
// export — every branch stock's stock_branch equals its branch_code, and the one unclaimed
// stock is exactly the one saying 'center' — but the string is a label one tenant happened
// to type, and reading it filed 566 warehouse lots under unknown_in_data as if 'center' were
// a mistyped branch code.

// branchRegistryWithStocks is the same nine branches, with the stock_id column populated
// using the real ObjectIds from the export.
func branchRegistryWithStocks() []byte {
	return []byte("INSERT INTO `branch` (`branch_code`, `branch_name`, `branch_name_th`, " +
		"`branch_tax_identification`, `active`, `stock_id`) VALUES\n" +
		"('00', 'แผนกการตลาด', '', '', 1, '6951146297539e2d28bc0a22'),\n" +
		"('01', 'สาขา พระราม 9', 'บริษัท แอนจาลี จำกัด', '0105558054530', 1, '63608c6c7796354dbae69c55'),\n" +
		"('02', 'สาขา เกษตรนวมินทร์', 'บจก. แอนจาลี', '0105558054530', 1, '63608c99e3553b4da0676c4d'),\n" +
		"('03', 'สาขา ราชพฤกษ์', 'บริษัท เอเจสองสี่ จำกัด', '0105566027191', 1, '64128720030d9b36ce99cd57'),\n" +
		"('04', 'สาขา ชิดลม', 'บริษัท แอนจาลี จำกัด', '0105558054530', 1, '660a55b65a0fdc487be92ba2'),\n" +
		"('06', 'สาขา รังสิต', 'บริษัท เอเจห้าหก จำกัด', '0135568021863', 1, '68c7c642974f3dbb950d44ab');\n")
}

func TestStockOwnersReadsEachBranchesStockFromTheBranchTable(t *testing.T) {
	got, err := stockOwners(Input{BranchDump: branchRegistryWithStocks()})
	if err != nil {
		t.Fatalf("stockOwners: %v", err)
	}
	want := map[string]string{
		"6951146297539e2d28bc0a22": "00",
		"63608c6c7796354dbae69c55": "01",
		"63608c99e3553b4da0676c4d": "02",
		"64128720030d9b36ce99cd57": "03",
		"660a55b65a0fdc487be92ba2": "04",
		"68c7c642974f3dbb950d44ab": "06",
	}
	if len(got) != len(want) {
		t.Fatalf("stockOwners returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for id, code := range want {
		if got[id] != code {
			t.Errorf("stock %s → %q, want %q", id, got[id], code)
		}
	}
	// A stock nobody listed must be absent rather than mapped to anything — that absence is
	// what identifies the central warehouse.
	if code, present := got["5fab83eebb59bca34e513211"]; present {
		t.Errorf("the warehouse stock resolved to branch %q; it must not be in the map at all", code)
	}
}

func TestStockOwnersWithNoBranchTableCannotTellRatherThanGuessing(t *testing.T) {
	// An empty map must mean "no information". A caller that read it as "no branch owns
	// anything" would send every lot to the warehouse.
	got, err := stockOwners(Input{})
	if err != nil {
		t.Fatalf("stockOwners with no dump must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries from no branch table, want 0", len(got))
	}
}

func TestKnowsBranchTellsWhetherTheBranchWasCreatedInThisClinic(t *testing.T) {
	// The tick list decides this. A branch left out is being imported as its own clinic, so
	// its rows have no home here.
	in := Input{BranchMap: map[string]string{
		"00": "BR00001", "01": "BR00002", "02": "BR00003",
		"04": "BR00004", "05": "BR00005", "07": "BR00006", "08": "BR00007",
	}}
	for _, ticked := range []string{"00", "01", "02", "04", "05", "07", "08"} {
		if !knowsBranch(in, ticked) {
			t.Errorf("branch %q was created but knowsBranch said no", ticked)
		}
	}
	for _, unticked := range []string{"03", "06"} {
		if knowsBranch(in, unticked) {
			t.Errorf("branch %q was NOT created but knowsBranch said yes", unticked)
		}
	}
}

func TestKnowsBranchWithNoMapKeepsTheSingleBranchImportWorking(t *testing.T) {
	// Every import before branch mapping supplied no map and sent everything to the one
	// branch that existed. Answering false here would start dropping lots from imports that
	// used to work.
	in := Input{}
	for _, any := range []string{"01", "03", "center", "", "99"} {
		if !knowsBranch(in, any) {
			t.Errorf("with no branch map, knowsBranch(%q) = false — it must not drop anything", any)
		}
	}
}

func TestSurveyCountsWarehouseLotsApartFromBranchLotsAndNotAsErrors(t *testing.T) {
	// Lots live in the Mongo `items` collection, one document per lot, each pointing at the
	// stock it sits in. Three of these belong to branches, two to a stock no branch claims.
	mongo := []byte(`db.getCollection("items").insert([
  { _id: ObjectId("aa0000000000000000000001"), code: "D001", stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("aa0000000000000000000002"), code: "D002", stock: ObjectId("63608c6c7796354dbae69c55") },
  { _id: ObjectId("aa0000000000000000000003"), code: "D003", stock: ObjectId("660a55b65a0fdc487be92ba2") },
  { _id: ObjectId("aa0000000000000000000004"), code: "D004", stock: ObjectId("5fab83eebb59bca34e513211") },
  { _id: ObjectId("aa0000000000000000000005"), code: "D005", stock: ObjectId("5fab83eebb59bca34e513211") }
]);
`)

	s, err := SurveyBranches(Input{BranchDump: branchRegistryWithStocks(), MongoDump: mongo})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}

	byCode := map[string]BranchInfo{}
	for _, b := range s.Branches {
		byCode[b.Code] = b
	}
	if byCode["01"].StockLots != 2 {
		t.Errorf("branch 01 lots = %d, want 2", byCode["01"].StockLots)
	}
	if byCode["04"].StockLots != 1 {
		t.Errorf("branch 04 lots = %d, want 1", byCode["04"].StockLots)
	}
	if s.MainStockLots != 2 {
		t.Errorf("main_stock_lots = %d, want 2", s.MainStockLots)
	}

	// The decisive assertion: the warehouse must NOT be reported as a mistyped branch code.
	// Reading stocks.stock_branch put 'center' here with 566 rows against it.
	for code := range s.UnknownInData {
		if code == "center" || code == "5fab83eebb59bca34e513211" {
			t.Errorf("the warehouse appears in unknown_in_data as %q — it is not a data error", code)
		}
	}

	if !strings.Contains(s.Text(), "คลังหลัก") {
		t.Error("the printed survey does not mention the warehouse lots")
	}
}

func TestSurveyDoesNotClassifyLotsWhenTheBranchRegistryIsMissing(t *testing.T) {
	// With no registry there is nothing to compare a stock against. Calling every lot
	// central would be a guess presented as a finding.
	mongo := []byte(`db.getCollection("items").insert([
  { _id: ObjectId("aa0000000000000000000001"), code: "D001", stock: ObjectId("63608c6c7796354dbae69c55") }
]);
`)
	s, err := SurveyBranches(Input{MongoDump: mongo})
	if err != nil {
		t.Fatalf("survey: %v", err)
	}
	if s.MainStockLots != 0 {
		t.Errorf("main_stock_lots = %d with no registry, want 0 — the answer is unknown, not zero-branch",
			s.MainStockLots)
	}
}
