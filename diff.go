package cometgo

import (
	"bytes"
	"encoding/json"
	"slices"
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
func diffResults(old, new *result, ordered bool) []op {
	var ops []op
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
		return ops
	}

	newPos := make(map[string]int, len(new.keys))
	for i, k := range new.keys {
		newPos[k] = i
	}
	seq := make([]int, len(cur))
	for i, k := range cur {
		seq[i] = newPos[k]
	}
	stable := make(map[string]bool, len(cur))
	for _, i := range lis(seq) {
		stable[cur[i]] = true
	}

	for j, k := range new.keys {
		oldRow, existed := old.rows[k]
		if existed && stable[k] {
			if p := patchOf(k, oldRow, new.rows[k]); p != nil {
				ops = append(ops, *p)
			}
			continue
		}
		if existed {
			cur = slices.Delete(cur, slices.Index(cur, k), slices.Index(cur, k)+1)
		}
		at := 0
		if j > 0 {
			at = slices.Index(cur, new.keys[j-1]) + 1
		}
		cur = slices.Insert(cur, at, k)
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
	return ops
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
