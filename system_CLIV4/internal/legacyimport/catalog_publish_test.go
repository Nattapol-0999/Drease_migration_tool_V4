package legacyimport

// The storefront switch, for services and courses only.
//
// WHY THIS IS NOT OBVIOUS FROM READING THE SQL
//
// Nothing in the import was setting catalog_item_channel.published to false. The rows did not
// exist at all, and the screen was showing the column default — so "why is the toggle off" has
// an answer that no amount of staring at an UPDATE would have found. Measured on agafar-clinic
// after a successful import: catalog_channel 0 rows, catalog_item_channel 0 rows. Migration 0263
// creates the `online` channel only for clinics that had catalog_item rows AT THE TIME IT RAN,
// which is no clinic imported afterwards, so the channel has to be created here too.
//
// WHAT THESE TESTS PROTECT
//
//  1. Scope. Owner chose services and courses and explicitly not drugs (18 Sep 2026). A later
//     "publish everything" is one word away and would put a pharmacy list on a public page.
//  2. DO NOTHING, not DO UPDATE. Once a row exists, a human has decided that item's publish
//     state on screen. A re-import that overruled them would be silent and unexplainable.
//  3. The tax warning. Every row this publishes has empty tax fields, and that has to stay
//     loud — it is the one thing about this change that can produce a wrong document.

import (
	"strings"
	"testing"
)

func publishSQL(t *testing.T) (string, *Report) {
	t.Helper()
	rep := NewReport("C0TEST1")
	return buildCatalogPublish(Input{ClinicCode: "C0TEST1"}, rep), rep
}

func TestPublishCoversServicesAndCoursesButNotDrugs(t *testing.T) {
	sql, _ := publishSQL(t)

	if !strings.Contains(sql, "'service', 'course'") {
		t.Errorf("the item_type filter is not (service, course):\n%s", sql)
	}
	// The literal that would mean drugs went up too. Checked as an absence because the failure
	// mode is a widened IN list that still looks perfectly reasonable in a diff.
	if strings.Contains(sql, "'drug'") {
		t.Error("drugs are being published — the owner chose services and courses only")
	}
	if !strings.Contains(sql, "published, published_at") || !strings.Contains(sql, "true, now()") {
		t.Errorf("published is not being set to true with a timestamp:\n%s", sql)
	}
}

func TestPublishCreatesTheChannelBecauseAnImportedClinicHasNone(t *testing.T) {
	sql, _ := publishSQL(t)

	if !strings.Contains(sql, "INSERT INTO catalog_channel") {
		t.Error("the online channel is not created — an imported clinic has no channel row, " +
			"so the publish INSERT would join against nothing and write zero rows in silence")
	}
	if !strings.Contains(sql, "'online'") {
		t.Error("the channel key is not 'online', which is the one the catalog screen reads")
	}
	// Re-import must be a no-op rather than a duplicate-key failure.
	if !strings.Contains(sql, "ON CONFLICT (clinic_code, channel_key) DO NOTHING") {
		t.Errorf("the channel insert is not idempotent:\n%s", sql)
	}
}

func TestPublishNeverOverrulesAHumanWhoAlreadySetIt(t *testing.T) {
	sql, _ := publishSQL(t)

	if !strings.Contains(sql, "ON CONFLICT (item_id, channel_id) DO NOTHING") {
		t.Errorf("expected DO NOTHING on the item/channel pair:\n%s", sql)
	}
	if strings.Contains(sql, "ON CONFLICT (item_id, channel_id) DO UPDATE") {
		t.Error("DO UPDATE would let a re-import switch an item back on after somebody " +
			"deliberately switched it off on screen")
	}
}

func TestPublishSaysOutLoudThatTaxIsMissing(t *testing.T) {
	sql, rep := publishSQL(t)

	// Reported to the operator by the generator...
	if !hasText(rep.Warnings, "ภาษี") {
		t.Errorf("publishing priced items with no tax data was not warned about: %v", rep.Warnings)
	}
	// ...and counted by the database, which is the only side that can know how many.
	if !strings.Contains(sql, "n_notax") {
		t.Errorf("the script does not count how many published rows have no tax answer:\n%s", sql)
	}
	if !strings.Contains(sql, "tax_treatment") || !strings.Contains(sql, "tax_item_kind") {
		t.Errorf("the tax count checks the wrong columns:\n%s", sql)
	}
}

func TestPublishRunsAfterTheRowsItReads(t *testing.T) {
	// It selects catalog_item by id, and those ids are issued by the INSERTs in the service and
	// course builders. If this ever moves earlier in catalogOrder it writes nothing at all —
	// and writes nothing SUCCESSFULLY, which is the kind of failure that ships.
	iService, iCourse, iPublish := -1, -1, -1
	for i, name := range catalogOrder {
		switch name {
		case EntCatalogService:
			iService = i
		case EntCatalogCourse:
			iCourse = i
		case EntCatalogPublish:
			iPublish = i
		}
	}
	if iPublish < 0 {
		t.Fatal("the publish entity is not in catalogOrder, so it is never emitted")
	}
	if iPublish < iService || iPublish < iCourse {
		t.Errorf("publish is at %d, before service (%d) or course (%d) — it would select rows "+
			"that do not exist yet and report success", iPublish, iService, iCourse)
	}
}
