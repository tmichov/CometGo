package cometgo

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// result is an observer's current result set.
type result struct {
	keys []string                     // row order (query order)
	rows map[string][]json.RawMessage // key → encoded cells, positional per cols
}

// opKind values match the wire op tags (PROTOCOL.md §4.2).
const (
	opInsert = 'i'
	opUpdate = 'u'
	opDelete = 'd'
	opMove   = 'm'
)

// op is one row operation. Ops are applied sequentially; idx (when hasIdx)
// is the row's index after the op is applied.
type op struct {
	kind   byte
	key    string
	row    []json.RawMessage // insert: full row
	patch  []int             // update: changed column indexes
	cells  []json.RawMessage // update: values, parallel to patch
	idx    int
	hasIdx bool
}

func (o op) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`["`)
	b.WriteByte(o.kind)
	b.WriteString(`",`)
	b.Write(quote(o.key))
	switch o.kind {
	case opInsert:
		b.WriteByte(',')
		writeCells(&b, o.row)
	case opUpdate:
		b.WriteString(",[")
		for i, c := range o.patch {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.Itoa(c))
			b.WriteByte(',')
			b.Write(o.cells[i])
		}
		b.WriteByte(']')
	}
	if o.hasIdx {
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(o.idx))
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

func writeCells(b *bytes.Buffer, cells []json.RawMessage) {
	b.WriteByte('[')
	for i, c := range cells {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(c)
	}
	b.WriteByte(']')
}

// snapshotOps returns the ops that build r from an empty result.
func snapshotOps(r *result, ordered bool) []op {
	ops := make([]op, 0, len(r.keys))
	for i, k := range r.keys {
		ops = append(ops, op{kind: opInsert, key: k, row: r.rows[k], idx: i, hasIdx: ordered})
	}
	return ops
}

// resetMoves is the fewest moved rows for which diffResults resets instead
// of diffing, when they are also most of the surviving rows. Below it the
// moves are few, and cheaper to send than a snapshot.
const resetMoves = 64

// diffResults returns ops that transform old into new.
//
// Unordered: deletes, then inserts and patches, no positions.
//
// Ordered: rows whose relative order is unchanged (a longest increasing
// subsequence of old positions mapped into new order) stay put. Every other
// surviving row is moved, and every new row inserted, in ascending order of
// its final position, each placed directly after its final predecessor. A
// placed row stays adjacent to its predecessor because later placements only
// ever go after their own predecessor, so the sequence ends exactly at new.
//
// That also fixes every row's place among the others in advance: the rows
// placed between stable row s and the next stable row form one run directly
// after s, ahead of any not-yet-moved row that sat after s. So each row gets
// a fixed slot, and a row's current index is a count of occupied slots
// before it (a Fenwick tree), O(log n) per placement.
//
// When most rows move (see resetMoves) diffResults returns reset instead of
// ops: the caller sends a fresh snapshot, which is linear to build and
// apply.
func diffResults(old, new *result, ordered bool) (ops []op, reset bool) {
	cur := make([]string, 0, len(old.keys))
	for _, k := range old.keys {
		if _, ok := new.rows[k]; ok {
			cur = append(cur, k)
		} else {
			ops = append(ops, op{kind: opDelete, key: k})
		}
	}

	if !ordered {
		for _, k := range new.keys {
			oldRow, existed := old.rows[k]
			if !existed {
				ops = append(ops, op{kind: opInsert, key: k, row: new.rows[k]})
			} else if p := patchOf(k, oldRow, new.rows[k]); p != nil {
				ops = append(ops, *p)
			}
		}
		return ops, false
	}

	newPos := make(map[string]int, len(new.keys))
	for i, k := range new.keys {
		newPos[k] = i
	}
	seq := make([]int, len(cur))
	for i, k := range cur {
		seq[i] = newPos[k]
	}
	keep := lis(seq)
	if moved := len(cur) - len(keep); moved >= resetMoves && moved > len(cur)/2 {
		return nil, true
	}
	curIdx := make(map[string]int, len(cur))
	for i, k := range cur {
		curIdx[k] = i
	}
	stable := make([]bool, len(cur))
	for _, i := range keep {
		stable[i] = true
	}
	stableAt := func(k string) (int, bool) {
		i, ok := curIdx[k]
		return i, ok && stable[i]
	}

	// Slots, in cur order: the run placed before the first stable row, then
	// each old row (stable or moved) followed by the run placed after it if
	// it is stable. run[i+1] is the length of the run after cur[i].
	run := make([]int, len(cur)+1)
	anchor := -1
	for _, k := range new.keys {
		if i, ok := stableAt(k); ok {
			anchor = i
		} else {
			run[anchor+1]++
		}
	}
	oldSlot := make([]int, len(cur))
	n := run[0]
	for i := range cur {
		oldSlot[i] = n
		n += 1 + run[i+1]
	}
	occupied := newFenwick(n)
	for i := range cur {
		occupied.add(oldSlot[i], 1)
	}

	slot := -1 // slot of new.keys[j-1]
	for j, k := range new.keys {
		oldRow, existed := old.rows[k]
		if i, ok := stableAt(k); ok {
			slot = oldSlot[i]
			if p := patchOf(k, oldRow, new.rows[k]); p != nil {
				ops = append(ops, *p)
			}
			continue
		}
		if existed {
			occupied.add(oldSlot[curIdx[k]], -1)
		}
		at := 0
		if j > 0 {
			at = occupied.prefix(slot) // index of the predecessor, plus one
		}
		slot++ // the run after the predecessor
		occupied.add(slot, 1)
		switch {
		case !existed:
			ops = append(ops, op{kind: opInsert, key: k, row: new.rows[k], idx: at, hasIdx: true})
		default:
			if p := patchOf(k, oldRow, new.rows[k]); p != nil {
				p.idx, p.hasIdx = at, true
				ops = append(ops, *p)
			} else {
				ops = append(ops, op{kind: opMove, key: k, idx: at, hasIdx: true})
			}
		}
	}
	return ops, false
}

func patchOf(key string, oldRow, newRow []json.RawMessage) *op {
	var p op
	for i := range newRow {
		if i >= len(oldRow) || !bytes.Equal(oldRow[i], newRow[i]) {
			p.patch = append(p.patch, i)
			p.cells = append(p.cells, newRow[i])
		}
	}
	if p.patch == nil {
		return nil
	}
	p.kind, p.key = opUpdate, key
	return &p
}

// lis returns the indexes of one longest strictly increasing subsequence of s.
func lis(s []int) []int {
	if len(s) == 0 {
		return nil
	}
	tails := []int{} // tails[l] = index in s of smallest tail of an increasing run of length l+1
	prev := make([]int, len(s))
	for i, v := range s {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if s[tails[mid]] < v {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo > 0 {
			prev[i] = tails[lo-1]
		} else {
			prev[i] = -1
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	out := make([]int, len(tails))
	for i, k := len(tails)-1, tails[len(tails)-1]; i >= 0; i, k = i-1, prev[k] {
		out[i] = k
	}
	return out
}

// fenwick counts occupied slots: add marks or clears one, prefix counts those
// at or before a slot. Both are O(log n).
type fenwick []int

func newFenwick(n int) fenwick { return make(fenwick, n+1) }

func (f fenwick) add(i, d int) {
	for i++; i < len(f); i += i & -i {
		f[i] += d
	}
}

func (f fenwick) prefix(i int) int {
	s := 0
	for i++; i > 0; i -= i & -i {
		s += f[i]
	}
	return s
}
