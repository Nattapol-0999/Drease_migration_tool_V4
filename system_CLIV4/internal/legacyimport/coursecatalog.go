package legacyimport

import (
	"fmt"
	"strings"
)

// How a customer's course balance finds its course in the target catalog.
//
// THE PROBLEM THIS SOLVES
//
// The legacy `course_items` collection carries NO code of its own, so every importer has
// to invent one. This tool mints CS001…; the catalog already at the target was built by an
// earlier pipeline that minted CS00001…. Neither string can find the other.
//
// The obvious repair — line them up by POSITION, since both mint in dump order — was
// measured against the live database and REJECTED: of the 96 courses in the export, only
// 42 sit at the same position as the target's. Using position would have attached 54
// customers' balances to a course they never bought, and nothing in the system would have
// said so. Matching the course's own NAME finds the same 42, which is the honest answer:
// the target's catalog is a DIFFERENT snapshot, and 54 of these courses are simply not in
// it.
//
// WHY A NULL HERE IS ACCEPTABLE AND A GUESS IS NOT
//
// course_bought.catalog_item_id is a REPORTING link. The deduction path never reads it:
// ListCourseBoughtItemsForUpdate LEFT JOINs catalog_item and wraps the id in
// COALESCE(…, 0), and the balance it enforces comes from course_bought_items — which
// resolve completely, because the treatments inside a course carry real LH codes from the
// source. So an unresolved course costs the clinic a grouped report; a WRONG one puts a
// customer's money against the wrong product.

// resolveCourseCatalogID renders the SQL that finds the catalog course, trying the most
// trustworthy evidence first:
//
//  1. the minted code — exact, and correct whenever the catalog came from this same tool
//  2. the course's own catalog name (course_items.name_th)
//  3. the name written on the bill at sale time (course_bills.name)
//
// Each name lookup must be UNAMBIGUOUS. `max(ci.id) … HAVING count(*) = 1` returns the id
// only when exactly one catalog course carries that name and NULL when two do, so two
// courses sharing a name can never silently claim each other's customers.
func resolveCourseCatalogID(clinic, code, catalogName, saleName string) string {
	byName := func(name string) string {
		if strings.TrimSpace(name) == "" {
			return ""
		}
		return fmt.Sprintf(`(SELECT max(ci.id) FROM catalog_item ci
             WHERE ci.clinic_code = %s AND ci.item_type = 'course'
               AND ci.status IN (1, 2) AND NOT ci.code_conflict
               AND lower(btrim(ci.name_th)) = lower(btrim(%s))
             HAVING count(*) = 1)`, lit(clinic), lit(name))
	}

	parts := []string{catalogItemIDByCode(clinic, code)}
	if e := byName(catalogName); e != "" {
		parts = append(parts, e)
	}
	// The sale-time name is only worth a second lookup when it differs from the catalog
	// name; when they are the same string it would repeat the query for nothing.
	if !strings.EqualFold(strings.TrimSpace(saleName), strings.TrimSpace(catalogName)) {
		if e := byName(saleName); e != "" {
			parts = append(parts, e)
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "COALESCE(" + strings.Join(parts, ",\n           ") + ")"
}

// courseCodeOfResolved renders the code that BELONGS to whichever catalog course was
// resolved, or NULL when none was.
//
// Writing the minted code instead would put a string like 'CS004' in the row that exists
// nowhere at the target — a code that looks real, joins to nothing, and would send the
// next person looking for a course that was never there.
func courseCodeOfResolved(clinic, code, catalogName, saleName string) string {
	return fmt.Sprintf("(SELECT ci.code FROM catalog_item ci WHERE ci.clinic_code = %s AND ci.id = %s)",
		lit(clinic), resolveCourseCatalogID(clinic, code, catalogName, saleName))
}
