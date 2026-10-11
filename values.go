package cometgo

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// colSpec describes one result column and how its values go on the wire
// (PROTOCOL.md §10).
type colSpec struct {
	name string
	code string // wire type code, e.g. "i8", "tstz", "[]text"
	oid  uint32
}

// typeCode maps a Postgres type OID to its wire type code.
func typeCode(m *pgtype.Map, oid uint32) string {
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID:
		return "i4"
	case pgtype.Int8OID:
		return "i8"
	case pgtype.Float4OID, pgtype.Float8OID:
		return "f8"
	case pgtype.NumericOID:
		return "num"
	case pgtype.BoolOID:
		return "bool"
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID:
		return "text"
	case pgtype.UUIDOID:
		return "uuid"
	case pgtype.TimestamptzOID:
		return "tstz"
	case pgtype.TimestampOID:
		return "ts"
	case pgtype.DateOID:
		return "date"
	case pgtype.JSONOID, pgtype.JSONBOID:
		return "json"
	case pgtype.ByteaOID:
		return "bytes"
	}
	t, ok := m.TypeForOID(oid)
	if !ok {
		// Types pgx does not know arrive in text format as strings. User enums
		// are registered before this is reached (types.go).
		return "text"
	}
	switch c := t.Codec.(type) {
	case *pgtype.ArrayCodec:
		return "[]" + typeCode(m, c.ElementType.OID)
	case *pgtype.EnumCodec:
		return "enum"
	}
	return "raw"
}

// elemOID returns the element OID for an array type, or 0.
func elemOID(m *pgtype.Map, oid uint32) uint32 {
	if t, ok := m.TypeForOID(oid); ok {
		if c, ok := t.Codec.(*pgtype.ArrayCodec); ok {
			return c.ElementType.OID
		}
	}
	return 0
}

const (
	tstzLayout = "2006-01-02T15:04:05.000000Z"
	tsLayout   = "2006-01-02T15:04:05.000000"
	dateLayout = "2006-01-02"
)

// encodeCell encodes one decoded pgx value v as wire JSON for type code.
// raw is the undecoded column bytes in the given format; it is only used for
// top-level json/jsonb so their contents pass through byte-exact.
func encodeCell(m *pgtype.Map, code string, oid uint32, v any, raw []byte, format int16) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	if code == "json" && raw != nil {
		if format == pgtype.BinaryFormatCode && oid == pgtype.JSONBOID {
			if len(raw) == 0 || raw[0] != 1 {
				return nil, fmt.Errorf("unsupported jsonb binary version")
			}
			raw = raw[1:]
		}
		return compactJSON(raw)
	}
	if inf, ok := v.(pgtype.InfinityModifier); ok {
		switch inf {
		case pgtype.Infinity:
			return json.RawMessage(`"infinity"`), nil
		case pgtype.NegativeInfinity:
			return json.RawMessage(`"-infinity"`), nil
		}
	}
	if elem, ok := strings.CutPrefix(code, "[]"); ok {
		items, ok := v.([]any)
		if !ok {
			return textCell(m, oid, v)
		}
		eoid := elemOID(m, oid)
		out := []byte{'['}
		for i, it := range items {
			if i > 0 {
				out = append(out, ',')
			}
			c, err := encodeCell(m, elem, eoid, it, nil, 0)
			if err != nil {
				return nil, err
			}
			out = append(out, c...)
		}
		return append(out, ']'), nil
	}

	switch code {
	case "i4":
		switch n := v.(type) {
		case int16:
			return json.RawMessage(strconv.FormatInt(int64(n), 10)), nil
		case int32:
			return json.RawMessage(strconv.FormatInt(int64(n), 10)), nil
		case int64:
			return json.RawMessage(strconv.FormatInt(n, 10)), nil
		}
	case "i8":
		if n, ok := v.(int64); ok {
			return quote(strconv.FormatInt(n, 10)), nil
		}
	case "f8":
		var f float64
		switch n := v.(type) {
		case float32:
			f = float64(n)
		case float64:
			f = n
		default:
			return textCell(m, oid, v)
		}
		switch {
		case math.IsNaN(f):
			return json.RawMessage(`"NaN"`), nil
		case math.IsInf(f, 1):
			return json.RawMessage(`"Infinity"`), nil
		case math.IsInf(f, -1):
			return json.RawMessage(`"-Infinity"`), nil
		}
		return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64)), nil
	case "bool":
		if b, ok := v.(bool); ok {
			if b {
				return json.RawMessage("true"), nil
			}
			return json.RawMessage("false"), nil
		}
	case "text", "enum":
		if s, ok := v.(string); ok {
			return quote(s), nil
		}
	case "uuid":
		if b, ok := v.([16]byte); ok {
			return quote(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])), nil
		}
	case "tstz", "ts", "date":
		if t, ok := v.(time.Time); ok {
			switch code {
			case "tstz":
				return quote(t.UTC().Format(tstzLayout)), nil
			case "ts":
				return quote(t.Format(tsLayout)), nil
			default:
				return quote(t.Format(dateLayout)), nil
			}
		}
	case "bytes":
		if b, ok := v.([]byte); ok {
			return quote(base64.StdEncoding.EncodeToString(b)), nil
		}
	case "json":
		// json inside an array: pgx already decoded it.
		return json.Marshal(v)
	}
	// num, raw, and any unexpected Go type: Postgres text form as a string.
	return textCell(m, oid, v)
}

// textCell encodes v as its Postgres text representation in a JSON string.
func textCell(m *pgtype.Map, oid uint32, v any) (json.RawMessage, error) {
	if s, ok := v.(string); ok {
		return quote(s), nil
	}
	buf, err := m.Encode(oid, pgtype.TextFormatCode, v, nil)
	if err != nil {
		return nil, fmt.Errorf("encode oid %d as text: %w", oid, err)
	}
	return quote(string(buf)), nil
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func compactJSON(b []byte) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.Grow(len(b))
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rowKey builds the opaque canonical key string for a row (PROTOCOL.md §4.2):
// the wire string of a single-column key, or compact JSON array text of a
// composite key.
func rowKey(cells []json.RawMessage, keyIdx []int) (string, error) {
	if len(keyIdx) == 1 {
		c := cells[keyIdx[0]]
		if len(c) > 0 && c[0] == '"' {
			var s string
			if err := json.Unmarshal(c, &s); err != nil {
				return "", err
			}
			return s, nil
		}
		if string(c) == "null" {
			return "", fmt.Errorf("null primary key value")
		}
		return string(c), nil
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, k := range keyIdx {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(cells[k])
	}
	sb.WriteByte(']')
	return sb.String(), nil
}
