package legacyimport

// Tests for what reaches catalog_service.loyalty_points.
//
// V2's listheal.point holds two unrelated things: a loyalty rate at some clinics, and the
// stored-value credit a prepaid package hands back at others. Importing the second kind as
// loyalty points is not cosmetic — awardPoints sums loyalty_points over an invoice and
// awards it once the bill is settled in full, gated only by loyalty_policy.active with no
// per-bill ceiling. Anjali (C6C936C) arrived with 4,603,000 points across 19 Member
// packages and had to be cleaned by hand on 28 Sep 2026.
//
// The owner's rule: carry `point` only when it is smaller than the price. These tests pin
// it with the real rows from production, because the rule is only trustworthy if it sorts
// those correctly.

import (
	"strings"
	"testing"
)

func TestServiceLoyaltyPointsSeparatesCreditFromPoints(t *testing.T) {
	cases := []struct {
		name       string
		point      string
		price      string
		wantPoints int
		wantCredit bool
	}{
		// Real rows that are stored-value credit. Every one is named วงเงิน / Member / VIP /
		// Package แถม in the source, and every one has point >= price.
		{"member 600k gives 750k", "750000", "600000", 0, true},
		{"vip 500k gives 550k", "550000", "500000", 0, true},
		{"wanaka package 8k gives 9.6k", "9600", "8000", 0, true},
		{"vip 120,00 แถม 10,000", "43285", "33285", 0, true},
		// Equal counts as credit too: "Exosome 17,000" carried point 17,000, which is the
		// price copied into the field rather than a rate.
		{"point equals price", "17000", "17000", 0, true},
		{"deposit promo", "1000", "1000", 0, true},

		// Real rows that are plausible rates. All have point < price.
		{"10 points on a 10,000 service", "10", "10000", 10, false},
		{"1 point on a 10,000 service", "1", "10000", 1, false},
		{"500 points on a 5,000 service", "500", "5000", 500, false},
		{"1 point on a 50,000 vip", "1", "50000", 1, false},

		// Nothing to decide.
		{"no points at all", "0", "10000", 0, false},
		{"empty points", "", "10000", 0, false},
		{"unparsable points", "abc", "10000", 0, false},
		{"negative points", "-5", "10000", 0, false},

		// A price that cannot be compared falls on the refusing side. Such a service is
		// status=draft anyway and cannot be sold until someone prices it, which is the same
		// moment they would set its points.
		{"no price", "5", "", 0, true},
		{"zero price", "5", "0", 0, true},
		{"unparsable price", "5", "n/a", 0, true},

		// Rounding is on the price, so a fractional price cannot let credit through.
		{"point just under a fractional price", "17000", "17000.40", 0, true},
		{"point just over a fractional price", "17001", "17000.40", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			points, credit := serviceLoyaltyPoints(c.point, c.price)
			if points != c.wantPoints || credit != c.wantCredit {
				t.Errorf("serviceLoyaltyPoints(%q, %q) = (%d, %v), want (%d, %v)",
					c.point, c.price, points, credit, c.wantPoints, c.wantCredit)
			}
		})
	}
}

// serviceCreditSample is three services from Anjali's export: a Member package whose
// `point` is the credit it hands back, an ordinary service with a real rate, and the row
// the arithmetic cannot classify — "Beauty Credit" priced 20,000 with point 1,000.
const serviceCreditSample = `db.getCollection("listheals").insert([
  { _id: ObjectId("6a00000000000000000000c1"), code: "LH447",
    name_th: "Member 600,000 ได้ 750,000", cost: NumberDecimal("600000"),
    point: NumberDecimal("750000") },
  { _id: ObjectId("6a00000000000000000000c2"), code: "LH087",
    name_th: "Anti - Aging", cost: NumberDecimal("10000"), point: NumberDecimal("10") },
  { _id: ObjectId("6a00000000000000000000c3"), code: "LH491",
    name_th: "Beauty Credit", cost: NumberDecimal("20000"), point: NumberDecimal("1000") }
]);`

func TestCatalogServiceDropsCreditAndKeepsRealPoints(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(serviceCreditSample))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildCatalogServices(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("buildCatalogServices: %v", err)
	}

	// The credit package must not arrive as 750,000 loyalty points.
	if strings.Contains(sql, "750000, NULL, NULL") {
		t.Errorf("the Member package's credit was imported as loyalty points\n%s", sql)
	}
	// The real rate must survive.
	if !strings.Contains(sql, "10, NULL, NULL") {
		t.Errorf("the 10-point rate on Anti - Aging was dropped\n%s", sql)
	}

	// Refusing quietly would be worse than importing it: the clinic has to be told which
	// rows lost their number so it can set a real rate on screen.
	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "LH447") && strings.Contains(w, "เครดิตบาท") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning naming the refused package; warnings = %v", rep.Warnings)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "LH087") {
			t.Errorf("the service with a real rate should not be reported as credit: %s", w)
		}
	}
}

// The row the numbers cannot classify: named like a prepaid package, valued like a rate.
// It is imported as the source says and reported, because refusing on a word would delete
// an intention a clinic may really have stated. (Owner's rule, 28 Sep 2026.)
func TestCatalogServiceWarnsOnCreditNameButKeepsTheValue(t *testing.T) {
	mc, err := parseMongoCatalog([]byte(serviceCreditSample))
	if err != nil {
		t.Fatalf("parseMongoCatalog: %v", err)
	}
	rep := NewReport("C9C9611")
	sql, err := buildCatalogServices(mc, Input{ClinicCode: "C9C9611"}, rep)
	if err != nil {
		t.Fatalf("buildCatalogServices: %v", err)
	}

	// Kept, not zeroed: 1,000 < 20,000 so the arithmetic has nothing to object to.
	if !strings.Contains(sql, "1000, NULL, NULL") {
		t.Errorf("LH491 lost its points even though the value passed the rule\n%s", sql)
	}

	var warned bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "LH491") && strings.Contains(w, "นำเข้าตามต้นทาง") {
			warned = true
		}
		// It must not be reported as refused credit — that would say the wrong thing about
		// what the import did.
		if strings.Contains(w, "LH491") && strings.Contains(w, "จึงนำเข้าเป็น 0") {
			t.Errorf("LH491 was reported as refused, but it was imported: %s", w)
		}
	}
	if !warned {
		t.Errorf("no warning naming the ambiguous row; warnings = %v", rep.Warnings)
	}
}

func TestLooksLikeStoredValueName(t *testing.T) {
	yes := []string{
		"Beauty Credit", "beauty credit", "Member 600,000 ได้ 750,000",
		"VIP 500,000 ได้รับ 550,000", "Vip 50000 แถม 10000",
		"วงเงิน 250000 ได้ 25000", "มัดจำโปรโมชั่น 5000 บาท",
		"Wanaka Premium Package Member 10", "เครดิตสะสม",
	}
	for _, n := range yes {
		if !looksLikeStoredValueName(n) {
			t.Errorf("looksLikeStoredValueName(%q) = false, want true", n)
		}
	}
	no := []string{
		"Anti - Aging", "Exosome", "ฉีด Filler ทดสอบ", "รายการรักษา A3 (มีแต้มสะสม)",
		"Botox Allergan 100u", "",
	}
	for _, n := range no {
		if looksLikeStoredValueName(n) {
			t.Errorf("looksLikeStoredValueName(%q) = true, want false", n)
		}
	}
}
