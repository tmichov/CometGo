package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// User-defined enums are unknown OIDs to pgx: their values arrive in text
// format as strings, and arrays of them as one array literal. The engine
// asks the catalog what such a type is and reports enums as "enum" and
// arrays of them as "[]enum", parsing the literal itself. Nothing is
// registered on the pool's connections, so the app's own queries see pgx
// exactly as configured.

// resolveTypes looks up, in one query, the OIDs among fds that m does not
// know and that are not cached yet, and caches what they are. It reads the
// catalog on its own connection rather than in the tick's snapshot, so a
// type created since that snapshot began is still found. A type the
// catalog does not return is not cached and is looked up again next time.
//
// The callers already hold a connection, so with the pool exhausted (two
// overlapping callers on a two-connection pool, or user code holding the
// rest) the Acquire would wait on the others forever: it is bounded by
// Config.QueryTimeout. Every failure wraps errTypeLookup.
func (e *engine) resolveTypes(ctx context.Context, m *pgtype.Map, fds []pgconn.FieldDescription) error {
	var oids []uint32
	for _, fd := range fds {
		if _, known := m.TypeForOID(fd.DataTypeOID); known {
			continue
		}
		e.typesMu.Lock()
		_, cached := e.pgTypes[fd.DataTypeOID]
		e.typesMu.Unlock()
		if !cached {
			oids = append(oids, fd.DataTypeOID)
		}
	}
	if len(oids) == 0 {
		return nil
	}
	if err := e.lookupTypes(ctx, oids); err != nil {
		return fmt.Errorf("%w: %w", errTypeLookup, err)
	}
	return nil
}

// errTypeLookup marks a failed catalog lookup in resolveTypes: a problem
// with the pool or the catalog, not with the query being described.
var errTypeLookup = errors.New("look up column types")

func (e *engine) lookupTypes(ctx context.Context, oids []uint32) error {
	ctx, cancel := context.WithTimeout(ctx, e.a.cfg.QueryTimeout)
	defer cancel()
	// Acquired explicitly: pgxpool's Query leaks its connection when a
	// tracer panics, and the callers recover such panics.
	pc, err := e.a.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pc.Release()
	rows, err := pc.Query(ctx, `
		SELECT t.oid, t.typtype::text, COALESCE(el.typtype::text, ''), COALESCE(base.typtype::text, '')
		FROM pg_type t
		LEFT JOIN pg_type el ON el.oid = t.typelem AND t.typcategory = 'A'
		LEFT JOIN pg_type base ON base.oid = el.typbasetype AND el.typtype = 'd'
		WHERE t.oid = ANY($1)`, oids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var typ, elemTyp, baseTyp string
		if err := rows.Scan(&oid, &typ, &elemTyp, &baseTyp); err != nil {
			return err
		}
		code := ""
		switch {
		case typ == "e":
			code = "enum"
		case elemTyp == "e", elemTyp == "d" && baseTyp == "e":
			// An array of an enum, or of a domain over one (a scalar
			// domain column already arrives as its base type).
			code = "[]enum"
		}
		e.typesMu.Lock()
		e.pgTypes[oid] = code
		e.typesMu.Unlock()
	}
	return rows.Err()
}

// colCode is typeCode with the catalog's answer for types pgx does not know.
func (e *engine) colCode(m *pgtype.Map, oid uint32) string {
	if _, known := m.TypeForOID(oid); !known {
		e.typesMu.Lock()
		code := e.pgTypes[oid]
		e.typesMu.Unlock()
		if code != "" {
			return code
		}
	}
	return typeCode(m, oid)
}

// parseArrayLiteral parses a Postgres array literal of text elements, such
// as {happy,"it's complicated",NULL}, into nested []any of string and nil.
func parseArrayLiteral(s string) (any, error) {
	if strings.HasPrefix(s, "[") {
		// Explicit bounds, e.g. [0:1]={a,b}: drop them, keep the elements.
		i := strings.Index(s, "=")
		if i < 0 {
			return nil, fmt.Errorf("malformed array literal %q", s)
		}
		s = s[i+1:]
	}
	p := &arrayParser{s: s}
	v, err := p.array()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("malformed array literal %q", s)
	}
	return v, nil
}

type arrayParser struct {
	s string
	i int
}

func (p *arrayParser) array() ([]any, error) {
	if p.i >= len(p.s) || p.s[p.i] != '{' {
		return nil, fmt.Errorf("malformed array literal %q", p.s)
	}
	p.i++
	out := []any{}
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		v, err := p.element()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("malformed array literal %q", p.s)
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, nil
		default:
			return nil, fmt.Errorf("malformed array literal %q", p.s)
		}
	}
}

func (p *arrayParser) element() (any, error) {
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("malformed array literal %q", p.s)
	}
	switch p.s[p.i] {
	case '{':
		return p.array()
	case '"':
		p.i++
		var sb strings.Builder
		for p.i < len(p.s) {
			c := p.s[p.i]
			p.i++
			switch c {
			case '\\':
				if p.i >= len(p.s) {
					return nil, fmt.Errorf("malformed array literal %q", p.s)
				}
				sb.WriteByte(p.s[p.i])
				p.i++
			case '"':
				return sb.String(), nil
			default:
				sb.WriteByte(c)
			}
		}
		return nil, fmt.Errorf("malformed array literal %q", p.s)
	}
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
		p.i++
	}
	tok := strings.TrimSpace(p.s[start:p.i])
	if strings.EqualFold(tok, "NULL") {
		return nil, nil
	}
	return tok, nil
}

// encodeEnumArray encodes an enum array that arrived as a literal.
func encodeEnumArray(lit string) (json.RawMessage, error) {
	v, err := parseArrayLiteral(lit)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
