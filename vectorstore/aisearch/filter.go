package aisearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reserved vectorstore.Query.Filter keys understood by this backend. They are
// never translated into field predicates.
const (
	// QueryTextKey carries an optional natural-language query string. When
	// present (and non-empty) the request becomes a hybrid query: Azure AI
	// Search runs full-text (BM25) retrieval on searchable fields alongside
	// the vector query and fuses both rankings with Reciprocal Rank Fusion.
	QueryTextKey = "_text"
	// RawFilterKey carries a raw OData $filter expression (string) that is
	// AND-ed with the predicates generated from the other filter keys. Use
	// it for anything the map dialect cannot express (or, not, geo, ...).
	RawFilterKey = "_odata"
)

// ErrInvalidFilter is returned (wrapped) when Query.Filter cannot be
// translated into an OData expression.
var ErrInvalidFilter = errors.New("vectorstore/aisearch: invalid filter")

// fieldPathRE matches an OData field path: identifiers separated by '/'
// (sub-fields of complex types).
var fieldPathRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(/[A-Za-z_][A-Za-z0-9_]*)*$`)

// BuildFilter translates a golly filter map into an OData $filter
// expression. prefix is prepended (with '/') to every key — pass the
// metadata complex-field name, or "" when metadata keys are top-level index
// fields.
//
// Dialect (all top-level entries are AND-ed, keys in sorted order):
//
//	"k": "v"                        -> k eq 'v'          (strings, numbers, bools, time.Time)
//	"k": nil                        -> k eq null
//	"k": []any{"a", "b"}            -> (k eq 'a' or k eq 'b')
//	"k": map[string]any{"$ne": 1}   -> k ne 1
//	     $eq $ne $gt $gte $lt $lte  -> eq ne gt ge lt le
//	     $in / $nin (slice)         -> (k eq a or ...) / not (k eq a or ...)
//	     $any (scalar or slice)     -> k/any(t: t eq a [or t eq b])  (Collection fields)
//	     $exists (bool)             -> k ne null / k eq null
//	"_odata": "<raw expression>"    -> (<raw expression>)
//	"_text": "..."                  -> ignored here (hybrid search text)
//
// An empty result string means "no filter".
func BuildFilter(prefix string, filter map[string]any) (string, error) {
	if len(filter) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(filter))
	for k := range filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	clauses := make([]string, 0, len(keys))
	for _, k := range keys {
		v := filter[k]
		switch k {
		case QueryTextKey:
			continue
		case RawFilterKey:
			raw, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("%w: %q must be a string, got %T", ErrInvalidFilter, RawFilterKey, v)
			}
			if raw = strings.TrimSpace(raw); raw != "" {
				clauses = append(clauses, "("+raw+")")
			}
			continue
		}
		if !fieldPathRE.MatchString(k) {
			return "", fmt.Errorf("%w: key %q is not a valid OData field path", ErrInvalidFilter, k)
		}
		path := k
		if prefix != "" {
			path = prefix + "/" + k
		}
		c, err := fieldClause(path, v)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, c)
	}
	return strings.Join(clauses, " and "), nil
}

// fieldClause renders the predicate(s) for a single field.
func fieldClause(path string, v any) (string, error) {
	if ops, ok := v.(map[string]any); ok {
		if len(ops) == 0 {
			return "", fmt.Errorf("%w: empty operator map for %q", ErrInvalidFilter, path)
		}
		names := make([]string, 0, len(ops))
		for op := range ops {
			names = append(names, op)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, op := range names {
			p, err := opClause(path, op, ops[op])
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " and "), nil
	}
	if isSlice(v) {
		return opClause(path, "$in", v)
	}
	return opClause(path, "$eq", v)
}

var comparisonOps = map[string]string{
	"$eq":  "eq",
	"$ne":  "ne",
	"$gt":  "gt",
	"$gte": "ge",
	"$lt":  "lt",
	"$lte": "le",
}

func opClause(path, op string, v any) (string, error) {
	if odata, ok := comparisonOps[op]; ok {
		lit, err := literal(v)
		if err != nil {
			return "", fmt.Errorf("%w: %s %s: %v", ErrInvalidFilter, path, op, err)
		}
		return path + " " + odata + " " + lit, nil
	}
	switch op {
	case "$in", "$nin":
		vals, err := sliceValues(v)
		if err != nil {
			return "", fmt.Errorf("%w: %s %s: %v", ErrInvalidFilter, path, op, err)
		}
		if len(vals) == 0 {
			// x in () is always false; x not in () always true.
			if op == "$in" {
				return "false", nil
			}
			return "true", nil
		}
		or, err := orEquals(path, vals)
		if err != nil {
			return "", err
		}
		if op == "$nin" {
			return "not " + or, nil
		}
		return or, nil
	case "$any":
		vals := []any{v}
		if isSlice(v) {
			var err error
			if vals, err = sliceValues(v); err != nil {
				return "", fmt.Errorf("%w: %s $any: %v", ErrInvalidFilter, path, err)
			}
		}
		if len(vals) == 0 {
			return path + "/any()", nil
		}
		or, err := orEquals("t", vals)
		if err != nil {
			return "", err
		}
		return path + "/any(t: " + strings.TrimSuffix(strings.TrimPrefix(or, "("), ")") + ")", nil
	case "$exists":
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("%w: %s $exists expects a bool, got %T", ErrInvalidFilter, path, v)
		}
		if b {
			return path + " ne null", nil
		}
		return path + " eq null", nil
	default:
		return "", fmt.Errorf("%w: unsupported operator %q on %q", ErrInvalidFilter, op, path)
	}
}

// orEquals renders (path eq a or path eq b ...). Always parenthesised.
func orEquals(path string, vals []any) (string, error) {
	parts := make([]string, 0, len(vals))
	for _, e := range vals {
		lit, err := literal(e)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %v", ErrInvalidFilter, path, err)
		}
		parts = append(parts, path+" eq "+lit)
	}
	return "(" + strings.Join(parts, " or ") + ")", nil
}

// literal renders a Go value as an OData literal.
func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.FormatInt(int64(x), 10), nil
	case int8:
		return strconv.FormatInt(int64(x), 10), nil
	case int16:
		return strconv.FormatInt(int64(x), 10), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return formatFloat(float64(x))
	case float64:
		return formatFloat(x)
	case json.Number:
		if _, err := x.Float64(); err != nil {
			return "", fmt.Errorf("invalid number %q", x.String())
		}
		return x.String(), nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	default:
		return "", fmt.Errorf("unsupported value type %T", v)
	}
}

func formatFloat(f float64) (string, error) {
	switch {
	case math.IsNaN(f):
		return "NaN", nil
	case math.IsInf(f, 1):
		return "INF", nil
	case math.IsInf(f, -1):
		return "-INF", nil
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

func isSlice(v any) bool {
	if v == nil {
		return false
	}
	k := reflect.TypeOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// sliceValues converts any slice/array value into []any.
func sliceValues(v any) ([]any, error) {
	if !isSlice(v) {
		return nil, fmt.Errorf("expected a slice, got %T", v)
	}
	rv := reflect.ValueOf(v)
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, nil
}
