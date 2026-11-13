package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// DomainEvidence matches the Node migration verifier's domain(db): only nonempty
// tables are counted; rows use SQLite storage classes and safeIntegers wrappers,
// sort by JavaScript UTF-16 code units, then hash JSON([table,encodedRows]).
// This is distinct from physical DB/WAL hashes and schema admission authority.
type DomainEvidence struct {
	TableCounts       map[string]int64 `json:"tableCounts"`
	AuthorContentHash string           `json:"authorContentHash"`
}

var errDomainEncoding = errors.New("SCHEMA_DOMAIN_ENCODING_UNSUPPORTED")

func appendSQLiteValue(dst []byte, value any) ([]byte, error) {
	switch value := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case int64:
		dst = append(dst, `{"integer":"`...)
		dst = strconv.AppendInt(dst, value, 10)
		return append(dst, `"}`...), nil
	case float64:
		if math.IsInf(value, 0) || math.IsNaN(value) {
			return append(dst, "null"...), nil
		}
		if value == 0 {
			return append(dst, '0'), nil
		}
		encoded, err := json.Marshal(value)
		return append(dst, encoded...), err
	case bool:
		if value {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case string:
		if !utf8.ValidString(value) {
			return nil, errDomainEncoding
		}
		return appendJSString(dst, value), nil
	case []byte:
		// better-sqlite3 returns a Buffer, whose JSON serialization preserves bytes.
		dst = append(dst, `{"type":"Buffer","data":[`...)
		for i, b := range value {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendUint(dst, uint64(b), 10)
		}
		return append(dst, ']', '}'), nil
	default:
		return nil, errDomainEncoding
	}
}
func lessUTF16(left, right string) bool {
	// Compare UTF-16 units lazily: do not allocate full slices per comparison.
	a, b := jsUnits{text: left}, jsUnits{text: right}
	for {
		av, aok := a.next()
		bv, bok := b.next()
		if !aok || !bok {
			return !aok && bok
		}
		if av != bv {
			return av < bv
		}
	}
}

type jsUnits struct {
	text    string
	offset  int
	pending uint16
}

func (u *jsUnits) next() (uint16, bool) {
	if u.pending != 0 {
		v := u.pending
		u.pending = 0
		return v, true
	}
	if u.offset >= len(u.text) {
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(u.text[u.offset:])
	u.offset += size
	if r <= 0xffff {
		return uint16(r), true
	}
	hi, lo := utf16.EncodeRune(r)
	u.pending = uint16(lo)
	return uint16(hi), true
}

func logicalDomain(ctx context.Context, tx *sql.Tx, tables []string) (DomainEvidence, error) {
	result := DomainEvidence{TableCounts: make(map[string]int64)}
	digest := sha256.New()
	names := append([]string(nil), tables...)
	sort.Strings(names)
	for _, table := range names {
		sorter := newRowSorter(ctx)
		err := func() (retErr error) {
			defer func() { retErr = errors.Join(retErr, sorter.Close()) }()
			// Inspect column names without fetching bodies; reject conservatively sized
			// rows in SQLite before database/sql materializes potentially huge values.
			shape, err := tx.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(table)+" LIMIT 0")
			if err != nil {
				return err
			}
			columns, err := shape.Columns()
			closeErr := shape.Close()
			if err := errors.Join(err, closeErr); err != nil {
				return err
			}
			if err := preflightDomainRows(ctx, tx, table, columns); err != nil {
				return err
			}
			rows, err := tx.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(table))
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					return err
				}
				encoded := []byte{'['}
				for i, value := range values {
					if i > 0 {
						encoded = append(encoded, ',')
					}
					encoded, err = appendSQLiteValue(encoded, value)
					if err != nil {
						return err
					}
					if len(encoded) >= maxEncodedRowBytes {
						return errors.New("SCHEMA_DOMAIN_ROW_TOO_LARGE")
					}
				}
				encoded = append(encoded, ']')
				if err := sorter.Add(string(encoded)); err != nil {
					return err
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if sorter.count == 0 {
				return nil
			}
			result.TableCounts[table] = sorter.count
			_, _ = digest.Write(appendJSString([]byte{'['}, table))
			_, _ = digest.Write([]byte(",["))
			first := true
			if err := sorter.Visit(func(row string) error {
				if !first {
					_, _ = digest.Write([]byte{','})
				}
				first = false
				_, _ = digest.Write(appendJSString(nil, row))
				return nil
			}); err != nil {
				return err
			}
			_, _ = digest.Write([]byte("]]"))
			return nil
		}()
		if err != nil {
			return result, err
		}
	}
	result.AuthorContentHash = hex.EncodeToString(digest.Sum(nil))
	return result, nil
}
