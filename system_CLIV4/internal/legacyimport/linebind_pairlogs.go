package legacyimport

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"system_cliv4/internal/mongodump"
)

// mris.pair_logs as the SECOND source of LINE pairings (owner's decision ก, 6 Oct 2026).
//
// # THE RULE
//
// line_patients first. When the dump has no line_patients — or has it with no documents,
// which is what 10.0.142.33 holds — the pairings are reconstructed from pair_logs. When
// it has neither, the group is skipped rather than refused (build.go): a clinic that
// never used LINE pairing in V2 has nothing to carry over, and that is not an error.
//
// # WHAT pair_logs IS — measured on 10.0.133.87, 6 Oct 2026
//
// Not a list of pairings but the events behind them, one document per change:
//
//	action     CREATE 390 · DELETE 12 · SET_ACTIVE 22
//	type       PATIENT 420 · DOCTOR 4
//	user_code  the HN when type is PATIENT — 359 of that box's 360 line_patients pairs
//	           match a (user_code, line_id) in the log
//	line_id    the LINE user id
//	active     the state the event left the pairing in
//	createdAt  when it happened
//
// There is no hn field and no pair_branch.
//
// # HOW A CURRENT PAIRING IS RECOVERED
//
// Events are grouped by (user_code, line_id) and ordered by createdAt, then _id. The LAST
// event decides: the pairing is live when that event is not a DELETE and left active
// true. So a CREATE later DELETEd is not carried over, a CREATE left inactive and later
// SET_ACTIVE is, and a pairing deleted and created again is — at the time of its newest
// event.
//
// Measured against line_patients on the same box, the rule recovers the 359 pairs both
// sources share, and nothing the log says ended is still in line_patients. The two
// disagree on 3 pairs only: 2 the log says are live and line_patients does not hold, and
// 1 pairing older than the log itself (the log starts 2023-07-14).
//
// DOCTOR events are a doctor's own LINE account, not a patient's pairing, and are left
// out.

// pairLogRefPrefix marks a verified_ref that came from the log rather than from
// line_patients, so whoever traces a binding back knows which collection to open. The
// longest it makes the value is 10 + 24 characters, inside verified_ref's 64.
const pairLogRefPrefix = "pair_logs:"

// pairEvent is one pair_logs document, reduced to what decides the outcome.
type pairEvent struct {
	action string
	active bool
	at     time.Time
	hasAt  bool
	id     string
	pos    int // position in the dump, the last tie-break so the order is total
}

// eventBefore orders two events of the same pair: by time, then by _id, then by position.
//
// An event with no time sorts BEFORE one with a time — it cannot be the newest when
// another event says when it happened. _id is an ObjectId in hex, whose leading bytes
// are its creation second, so comparing the text compares insertion order.
func eventBefore(a, b pairEvent) bool {
	if a.hasAt != b.hasAt {
		return !a.hasAt
	}
	if a.hasAt && !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	if a.id != b.id {
		return a.id < b.id
	}
	return a.pos < b.pos
}

// bindsFromPairLogs reconstructs the live pairings from the event log.
//
// Every document is accounted for on the entity — as a binding, or as a skip with its
// reason — so SourceRows still adds up when the report is read.
func bindsFromPairLogs(docs []mongodump.Doc, e *EntityResult) (binds []lineBind, ended []string) {
	type pair struct{ hn, line string }
	events := map[pair][]pairEvent{}
	var order []pair

	for i, d := range docs {
		if !strings.EqualFold(strings.TrimSpace(d.Text("type")), "PATIENT") {
			e.skip("pair_logs type ไม่ใช่ PATIENT (เป็น LINE ของหมอ) — ไม่ใช่การผูกของคนไข้")
			continue
		}
		hn := strings.TrimSpace(d.Text("user_code"))
		lineID := strings.TrimSpace(d.Text("line_id"))
		if hn == "" {
			e.skip("pair_logs ไม่มี user_code — ไม่รู้ว่า LINE นี้เป็นของใคร")
			continue
		}
		if lineID == "" {
			e.skip("pair_logs ไม่มี line_id — ไม่มีอะไรให้ผูก")
			continue
		}

		ev := pairEvent{
			action: strings.ToUpper(strings.TrimSpace(d.Text("action"))),
			active: strings.TrimSpace(d.Text("active")) == "true",
			id:     docObjectID(d, "_id"),
			pos:    i,
		}
		if v, ok := d.Get("createdAt"); ok {
			if t, isTime := v.(time.Time); isTime {
				ev.at, ev.hasAt = t, true
			}
		}

		k := pair{hn, lineID}
		if _, seen := events[k]; !seen {
			order = append(order, k)
		}
		events[k] = append(events[k], ev)
	}

	for _, k := range order {
		evs := events[k]
		sort.SliceStable(evs, func(i, j int) bool { return eventBefore(evs[i], evs[j]) })

		// The earlier events of a pair are its history, not more pairings.
		for range evs[:len(evs)-1] {
			e.skip("pair_logs เหตุการณ์ก่อนหน้าของคู่เดียวกัน — ใช้เหตุการณ์ล่าสุดตัดสิน")
		}

		last := evs[len(evs)-1]
		if last.action == "DELETE" || !last.active {
			e.skip("pair_logs เหตุการณ์ล่าสุดคือยกเลิกการผูก (DELETE หรือ active = false) — ไม่นำเข้า")
			ended = append(ended, fmt.Sprintf("%s (%s)", k.hn, shortLineID(k.line)))
			continue
		}

		b := lineBind{
			hn:        k.hn,
			lineID:    k.line,
			active:    true,
			pairedAt:  last.at,
			hasPaired: last.hasAt,
		}
		if last.id != "" {
			b.srcID = pairLogRefPrefix + last.id
		}
		binds = append(binds, b)
	}
	return binds, ended
}
