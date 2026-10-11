package cometgo

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// pgTypeInfo is what the catalog says about a type pgx does not know.
type pgTypeInfo struct {
	enum bool   // a user-defined enum
	name string // its name, for registering it
	// elem is set for an array of a user-defined enum: the element type.
	elem *pgTypeInfo
	oid  uint32
}

// registrable reports whether the connection can be taught info's type.
func (info *pgTypeInfo) registrable() bool {
	return info.enum || info.elem != nil
}

// typesToRegister returns the OIDs among fds that m does not know and that
// are, or may be, enums or enum arrays: never looked up, or known to be
// one. Other unknown types stay as they are (text, §10) and cost nothing
// once looked up.
func (e *engine) typesToRegister(m *pgtype.Map, fds []pgconn.FieldDescription) []uint32 {
	var out []uint32
	for _, fd := range fds {
		if _, ok := m.TypeForOID(fd.DataTypeOID); ok {
			continue
		}
		if info, seen := e.pgTypes[fd.DataTypeOID]; seen && !info.registrable() {
			continue
		}
		out = append(out, fd.DataTypeOID)
	}
	return out
}

// registerTypes looks up the types in oids not yet in the cache, then
// registers the enums and enum arrays among them on m, so that typeCode
// reports them as enum and []enum and their values decode as strings.
func (e *engine) registerTypes(ctx context.Context, tx pgx.Tx, m *pgtype.Map, oids []uint32) error {
	for _, oid := range oids {
		info, seen := e.pgTypes[oid]
		if !seen {
			var err error
			if info, err = lookupType(ctx, tx, oid); err != nil {
				return err
			}
			e.pgTypes[oid] = info
		}
		switch {
		case info.enum:
			registerEnum(m, info)
		case info.elem != nil:
			elem := registerEnum(m, info.elem)
			m.RegisterType(&pgtype.Type{Name: "_" + info.elem.name, OID: info.oid, Codec: &pgtype.ArrayCodec{ElementType: elem}})
		}
	}
	return nil
}

func registerEnum(m *pgtype.Map, info *pgTypeInfo) *pgtype.Type {
	if t, ok := m.TypeForOID(info.oid); ok {
		return t
	}
	t := &pgtype.Type{Name: info.name, OID: info.oid, Codec: &pgtype.EnumCodec{}}
	m.RegisterType(t)
	return t
}

// lookupType reads oid's kind from pg_type: an enum, an array of an enum,
// or anything else.
func lookupType(ctx context.Context, tx pgx.Tx, oid uint32) (*pgTypeInfo, error) {
	var typtype, name, elemType, elemName string
	var elem uint32
	err := tx.QueryRow(ctx, `
		SELECT t.typtype::text, t.typname, COALESCE(e.oid, 0), COALESCE(e.typtype::text, ''), COALESCE(e.typname, '')
		FROM pg_type t
		LEFT JOIN pg_type e ON e.oid = t.typelem AND t.typcategory = 'A'
		WHERE t.oid = $1`, oid).Scan(&typtype, &name, &elem, &elemType, &elemName)
	if err == pgx.ErrNoRows {
		return &pgTypeInfo{oid: oid}, nil
	}
	if err != nil {
		return nil, err
	}
	info := &pgTypeInfo{oid: oid, name: name, enum: typtype == "e"}
	if elem != 0 && elemType == "e" {
		info.elem = &pgTypeInfo{oid: elem, name: elemName, enum: true}
	}
	return info, nil
}
