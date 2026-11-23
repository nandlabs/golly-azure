package cosmosdb

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"
)

// RawWhereKey is a reserved vectorstore.Query.Filter key carrying a raw
// Cosmos DB SQL predicate (string) that is AND-ed with the generated
// clauses. The item alias is "c" (e.g. "STARTSWITH(c.metadata.path, 'docs/')").
// It is passed through verbatim — never feed it untrusted input.
const RawWhereKey = "_sql"

// ErrInvalidFilter is returned (wrapped) when Query.Filter cannot be
// translated into a Cosmos DB SQL predicate.
var ErrInvalidFilter = errors.New("vectorstore/cosmosdb: invalid filter")

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sqlKeywords are Cosmos DB SQL reserved words that cannot be used with dot
// property access (c.value is a syntax error; c["value"] is fine).
var sqlKeywords = map[string]bool{
	"and": true, "array": true, "as": true, "asc": true, "between": true, "by": true,
	"case": true, "cast": true, "convert": true, "cross": true, "desc": true,
	"distinct": true, "else": true, "end": true, "escape": true, "exists": true,
	"false": true, "for": true, "from": true, "group": true, "having": true,
	"in": true, "inner": true, "insert": true, "into": true, "is": true, "join": true,
	"left": true, "like": true, "limit": true, "not": true, "null": true, "offset": true,
	"on": true, "or": true, "order": true, "outer": true, "over": true, "right": true,
	"select": true, "set": true, "then": true, "top": true, "true": true,
	"udf": true, "undefined": true, "update": true, "value": true, "when": true,
	"where": true, "with": true,
}

// prop renders a property access on base: base.name for plain identifiers,
// base["name"] otherwise (keywords, dashes, spaces, unicode, ...).
func prop(base, name string) string {
	if identRE.MatchString(name) && !sqlKeywords[strings.ToLower(name)] {
		return base + "." + name
	}
	q, _ := json.Marshal(name) // string marshalling never fails
	return base + "[" + string(q) + "]"
}

// whereBuilder accumulates parameterised predicates.
type whereBuilder struct {
	params []azcosmos.QueryParameter
	prefix string
}

func (b *whereBuilder) bind(v any) string {
	name := "@" + b.prefix + strconv.Itoa(len(b.params))
	b.params = append(b.params, azcosmos.QueryParameter{Name: name, Value: v})
	return name
}

// BuildWhere translates a golly filter map into a parameterised Cosmos DB
// SQL predicate over root (e.g. "c.metadata"). It returns the predicate
// (empty when there is nothing to filter) and the parameters it references
// (@f0, @f1, ...).
//
// Dialect (top-level entries AND-ed, keys in sorted order; values are
// always bound as parameters):
//
//	"k": v                          -> root.k = @f0
//	"k": nil                        -> IS_NULL(root.k)
//	"k": []any{a, b}                -> ARRAY_CONTAINS(@f0, root.k)
//	"k": map[string]any{"$gt": 1}   -> root.k > @f0
//	     $eq $ne $gt $gte $lt $lte  -> =  != >  >=  <  <=
//	     $in / $nin (slice)         -> ARRAY_CONTAINS(@p, root.k) / NOT ...
//	     $any (scalar or slice)     -> ARRAY_CONTAINS(root.k, @p) [OR ...]   (array-valued fields)
//	     $exists (bool)             -> IS_DEFINED(root.k) / NOT IS_DEFINED(root.k)
//	"_sql": "<raw predicate>"       -> (<raw predicate>)
func BuildWhere(root string, filter map[string]any) (string, []azcosmos.QueryParameter, error) {
	b := &whereBuilder{prefix: "f"}
	clause, err := b.build(root, filter)
	if err != nil {
		return "", nil, err
	}
	return clause, b.params, nil
}

func (b *whereBuilder) build(root string, filter map[string]any) (string, error) {
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
		if k == RawWhereKey {
			raw, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("%w: %q must be a string, got %T", ErrInvalidFilter, RawWhereKey, v)
			}
			if raw = strings.TrimSpace(raw); raw != "" {
				clauses = append(clauses, "("+raw+")")
			}
			continue
		}
		if k == "" {
			return "", fmt.Errorf("%w: empty key", ErrInvalidFilter)
		}
		c, err := b.field(prop(root, k), k, v)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, c)
	}
	return strings.Join(clauses, " AND "), nil
}

func (b *whereBuilder) field(path, key string, v any) (string, error) {
	if ops, ok := v.(map[string]any); ok {
		if len(ops) == 0 {
			return "", fmt.Errorf("%w: empty operator map for %q", ErrInvalidFilter, key)
		}
		names := make([]string, 0, len(ops))
		for op := range ops {
			names = append(names, op)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, op := range names {
			p, err := b.op(path, key, op, ops[op])
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " AND "), nil
	}
	if isSlice(v) {
		return b.op(path, key, "$in", v)
	}
	return b.op(path, key, "$eq", v)
}

var comparisonOps = map[string]string{
	"$eq":  "=",
	"$ne":  "!=",
	"$gt":  ">",
	"$gte": ">=",
	"$lt":  "<",
	"$lte": "<=",
}

func (b *whereBuilder) op(path, key, op string, v any) (string, error) {
	if sym, ok := comparisonOps[op]; ok {
		if v == nil {
			switch op {
			case "$eq":
				return "IS_NULL(" + path + ")", nil
			case "$ne":
				return "NOT IS_NULL(" + path + ")", nil
			}
			return "", fmt.Errorf("%w: %s on %q requires a non-nil value", ErrInvalidFilter, op, key)
		}
		if _, isMap := v.(map[string]any); isMap || isSlice(v) {
			return "", fmt.Errorf("%w: %s on %q requires a scalar, got %T", ErrInvalidFilter, op, key, v)
		}
		return path + " " + sym + " " + b.bind(v), nil
	}
	switch op {
	case "$in", "$nin":
		vals, err := sliceValues(v)
		if err != nil {
			return "", fmt.Errorf("%w: %s on %q: %v", ErrInvalidFilter, op, key, err)
		}
		c := "ARRAY_CONTAINS(" + b.bind(vals) + ", " + path + ")"
		if op == "$nin" {
			c = "NOT " + c
		}
		return c, nil
	case "$any":
		vals := []any{v}
		if isSlice(v) {
			vals, _ = sliceValues(v)
		}
		if len(vals) == 0 {
			return "false", nil
		}
		parts := make([]string, 0, len(vals))
		for _, e := range vals {
			parts = append(parts, "ARRAY_CONTAINS("+path+", "+b.bind(e)+")")
		}
		if len(parts) == 1 {
			return parts[0], nil
		}
		return "(" + strings.Join(parts, " OR ") + ")", nil
	case "$exists":
		ex, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("%w: $exists on %q expects a bool, got %T", ErrInvalidFilter, key, v)
		}
		if ex {
			return "IS_DEFINED(" + path + ")", nil
		}
		return "NOT IS_DEFINED(" + path + ")", nil
	default:
		return "", fmt.Errorf("%w: unsupported operator %q on %q", ErrInvalidFilter, op, key)
	}
}

func isSlice(v any) bool {
	if v == nil {
		return false
	}
	k := reflect.TypeOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

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
