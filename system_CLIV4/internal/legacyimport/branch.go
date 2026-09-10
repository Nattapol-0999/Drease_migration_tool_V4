package legacyimport

import (
	"fmt"
	"sort"
	"strings"

	"system_cliv4/internal/legacydump"
)

// Branch resolution: turning the legacy tenant's branch codes into V4's.
//
// WHY A MAPPING AND NOT THE SOURCE VALUE
// V4 owns its branch codes. They come out of a sequence ("BR00001"…) and each carries a
// branch_no that every HN issued at that branch keeps for life, so a row written with the
// legacy "01" would point at a branch that does not exist. The caller creates the branches
// first (POST /v3/import/clinics returns source_code → branch_code) and hands that map in.
//
// WHY THE OLD BEHAVIOUR IS STILL THE DEFAULT
// Before this existed every row was stamped with BranchCode ("01"), which was correct for
// the first tenants — their room master only ever used one branch. With no map supplied
// that is exactly what still happens, so an import that worked before behaves identically.

// branchResolver maps a legacy branch code to a V4 branch_code, and remembers what it
// could not map so the report can say so instead of a wrong branch going in unnoticed.
type branchResolver struct {
	m   map[string]string
	def string

	// unknown counts rows whose legacy code has no entry in the map; blank counts rows
	// with no branch at all. Both fall back to def, and both are reported.
	unknown map[string]int
	blank   int
	// mapped counts, per V4 branch_code, how many rows landed there.
	mapped map[string]int
}

func newBranchResolver(in Input) *branchResolver {
	def := strings.TrimSpace(in.DefaultBranch)
	if def == "" {
		// No caller-supplied default. When a map exists its first entry (sorted, so the
		// choice is stable across runs) is a far better guess than a code from another
		// system; with no map at all the historical constant is the only answer.
		if len(in.BranchMap) > 0 {
			keys := make([]string, 0, len(in.BranchMap))
			for k := range in.BranchMap {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			def = in.BranchMap[keys[0]]
		} else {
			def = BranchCode
		}
	}
	return &branchResolver{
		m:       in.BranchMap,
		def:     def,
		unknown: map[string]int{},
		mapped:  map[string]int{},
	}
}

// resolve returns the V4 branch_code for a legacy code. An empty or unmapped code gets
// the default, because branch_code is NOT NULL on every table that carries it (migration
// 0320) — refusing the row instead would drop patient history over a data-entry typo.
func (b *branchResolver) resolve(legacy string) string {
	code := strings.TrimSpace(legacy)
	if code == "" {
		b.blank++
		b.mapped[b.def]++
		return b.def
	}
	if len(b.m) == 0 {
		// Single-branch import: the source value is not a V4 code, so it cannot be used
		// as one. Everything goes to the one branch that exists.
		b.mapped[b.def]++
		return b.def
	}
	if v, ok := b.m[code]; ok {
		b.mapped[v]++
		return v
	}
	b.unknown[code]++
	b.mapped[b.def]++
	return b.def
}

// resolveOrKeep resolves a legacy code, or keeps an ALREADY-RESOLVED fallback when the
// legacy value is blank. It exists because a receipt's branch falls back to its bill's,
// and the bill's code has been through resolve() already — running it twice would look up
// a V4 code in a map keyed by legacy ones and land on the default.
func resolveOrKeep(b *branchResolver, legacy string, resolvedFallback string) string {
	if strings.TrimSpace(legacy) == "" {
		if strings.TrimSpace(resolvedFallback) != "" {
			return resolvedFallback
		}
		return b.resolve("")
	}
	return b.resolve(legacy)
}

// report writes what the resolver did into the import report. Called once per entity that
// resolves branches, with a label naming it, so a wrong-looking count can be traced to the
// table it came from.
func (b *branchResolver) report(rep *Report, label string) {
	if b.blank > 0 {
		rep.notef("%s: %d แถวไม่มีรหัสสาขาในต้นทาง — ใช้สาขา %s", label, b.blank, b.def)
	}
	if len(b.unknown) > 0 {
		codes := make([]string, 0, len(b.unknown))
		for c := range b.unknown {
			codes = append(codes, fmt.Sprintf("%s (%d แถว)", c, b.unknown[c]))
		}
		sort.Strings(codes)
		rep.warnf("%s: รหัสสาขาที่ไม่มีในรายการที่ส่งมา %s — ใช้สาขา %s แทน",
			label, strings.Join(codes, ", "), b.def)
	}
	if len(b.mapped) > 1 {
		parts := make([]string, 0, len(b.mapped))
		for code := range b.mapped {
			parts = append(parts, code)
		}
		sort.Strings(parts)
		counts := make([]string, 0, len(parts))
		for _, code := range parts {
			counts = append(counts, fmt.Sprintf("%s: %d", code, b.mapped[code]))
		}
		rep.notef("%s: แยกตามสาขา — %s", label, strings.Join(counts, " · "))
	}
}

/**
 * branchByHN indexes the branch each patient belongs to.
 *
 * The legacy `appointment` table has a branch_code column that is blank in every row of
 * the exports seen so far (20,972 of 20,972 in one tenant), while `patient.branch_code` is
 * NOT NULL and filled for all 14,264 patients — and no HN appears under two branches. So
 * the patient's own branch is the only real answer available for an appointment, and it is
 * an exact one rather than a guess.
 *
 * Returns legacy codes, not V4 ones: the caller resolves, so the counting and reporting
 * stay in one place.
 */
func branchByHN(rows []legacydump.Row) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		hn := strings.TrimSpace(r.Get("HN"))
		if hn == "" {
			continue
		}
		code := strings.TrimSpace(r.Get("branch_code"))
		if code == "" {
			continue
		}
		// First wins. A duplicate HN with a different branch would make this ambiguous;
		// the survey reports that case (cross_branch_hn) so it is known before import.
		if _, exists := out[hn]; !exists {
			out[hn] = code
		}
	}
	return out
}
