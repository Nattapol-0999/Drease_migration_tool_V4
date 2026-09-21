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

/**
 * stockOwners answers "which branch does this stock belong to" from the SQL side.
 *
 * The legacy `branch` table carries stock_id, and it holds the Mongo _id of that branch's
 * document in the `stocks` collection. That reference is the real statement of ownership,
 * so it is what decides where a lot lives:
 *
 *	lot.stock IS in branch.stock_id  → that branch's shelf
 *	lot.stock is NOT in it           → no branch claims it = the central warehouse
 *
 * The alternative was reading stocks.stock_branch, a string holding either a branch code or
 * 'center'. Both agree on the export this was written against — every branch stock's
 * stock_branch equals its branch_code, and the single unclaimed stock is exactly the one
 * saying 'center'. The reference is still the better rule: 'center' is a label this tenant
 * happened to type, and the next one may not.
 *
 * Returns stock_id → legacy branch_code. A missing or unparsable branch table yields an
 * empty map, which the caller must treat as "cannot tell", NOT as "everything is central".
 */
func stockOwners(in Input) (map[string]string, error) {
	if len(in.BranchDump) == 0 {
		return map[string]string{}, nil
	}
	_, rows, err := legacydump.Parse(string(in.BranchDump), "branch")
	if err != nil {
		return nil, fmt.Errorf("อ่านตาราง branch เพื่อหาคลังของแต่ละสาขาไม่ได้: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		stockID := strings.TrimSpace(r.Get("stock_id"))
		code := strings.TrimSpace(r.Get("branch_code"))
		if stockID == "" || code == "" {
			continue
		}
		out[stockID] = code
	}
	return out, nil
}

// knowsBranch reports whether the caller created this legacy branch in the target clinic.
//
// With no map at all this is the single-branch import that predates branch mapping, and
// every row goes to the one branch that exists — so it answers true and nothing changes.
func knowsBranch(in Input, legacy string) bool {
	if len(in.BranchMap) == 0 {
		return true
	}
	_, ok := in.BranchMap[strings.TrimSpace(legacy)]
	return ok
}

/*
firstBranchName is the tenant's OWN name for itself, used to build a login name that can
coexist with another clinic's identical one.

WHY `branch` AND NOT `clinic`

The legacy table called `clinic` is the ROOM master — its rows are `Room 1` / `ห้องตรวจ 1`
and V4 reads it as `room`. The tenant's name for itself is in `branch.branch_name`
("The Gloss Clinic. CCO"), which is the only place in the whole export that holds it.
Confirmed against the source on 21 Sep 2026 by reading both tables: `clinic` has no
clinic_name column at all, and `branch` has branch_name / branch_name_th / branch_name_en.

Owner chose branch_name (21 Sep 2026). Not branch_name_th, which is free to be Thai and
would leave nothing usable in an email local part.

# WHY THE FIRST BRANCH AND NOT THE DEFAULT ONE

Owner's decision, same day. An account belongs to the CLINIC, not to a branch, so any
per-branch choice would be arbitrary — and worse, unstable: -default-branch differs between
the sites of one tenant, so the same person would get a different login depending on which
site was being imported. The first branch_code is the same answer for every run.

Sorted by branch_code, not by file order: a dump is written in id order, and a branch added
later can sit before an older one once someone has edited the row.

A missing branch table is not an error here — it yields "" and the caller falls back to the
clinic code. This decides a NAME, and refusing an import over a cosmetic string would be the
wrong trade. A branch table that is present but unparsable IS returned as an error, because
that says the export is not what it claims to be.
*/
func firstBranchName(in Input) (string, error) {
	if len(in.BranchDump) == 0 {
		return "", nil
	}
	_, rows, err := legacydump.Parse(string(in.BranchDump), "branch")
	if err != nil {
		return "", fmt.Errorf("อ่านตาราง branch เพื่อหาชื่อคลินิกไม่ได้: %w", err)
	}
	type br struct{ code, name string }
	list := make([]br, 0, len(rows))
	for _, r := range rows {
		code := strings.TrimSpace(r.Get("branch_code"))
		name := strings.TrimSpace(r.Get("branch_name"))
		if code == "" || name == "" {
			continue
		}
		list = append(list, br{code: code, name: name})
	}
	if len(list) == 0 {
		return "", nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].code < list[j].code })
	return list[0].name, nil
}
