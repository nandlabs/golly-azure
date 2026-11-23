package aisearch

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestBuildFilter(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name   string
		prefix string
		filter map[string]any
		want   string
	}{
		{"empty", "metadata", nil, ""},
		{"only reserved text", "metadata", map[string]any{QueryTextKey: "x"}, ""},
		{"string eq", "metadata", map[string]any{"source": "O'Reilly"}, "metadata/source eq 'O''Reilly'"},
		{"no prefix", "", map[string]any{"source": "wiki"}, "source eq 'wiki'"},
		{"nested path", "metadata", map[string]any{"a/b": true}, "metadata/a/b eq true"},
		{"numbers", "", map[string]any{"i": 3, "f": 1.5, "u": uint8(2), "n": json.Number("7")}, "f eq 1.5 and i eq 3 and n eq 7 and u eq 2"},
		{"more ints", "", map[string]any{"a": int8(1), "b": int16(2), "c": int32(3), "d": int64(4), "e": uint(5), "g": uint16(6), "h": uint32(7), "j": uint64(8), "k": float32(0.5)},
			"a eq 1 and b eq 2 and c eq 3 and d eq 4 and e eq 5 and g eq 6 and h eq 7 and j eq 8 and k eq 0.5"},
		{"null", "", map[string]any{"x": nil}, "x eq null"},
		{"time", "", map[string]any{"t": map[string]any{"$gt": ts}}, "t gt 2024-01-02T03:04:05Z"},
		{"slice in", "", map[string]any{"tag": []string{"a", "b"}}, "(tag eq 'a' or tag eq 'b')"},
		{"ops", "", map[string]any{"r": map[string]any{"$gte": 1, "$lt": 5, "$ne": 3}}, "r ge 1 and r lt 5 and r ne 3"},
		{"eq lte", "", map[string]any{"r": map[string]any{"$eq": 1, "$lte": 2}}, "r eq 1 and r le 2"},
		{"in", "", map[string]any{"r": map[string]any{"$in": []any{1, 2}}}, "(r eq 1 or r eq 2)"},
		{"nin", "", map[string]any{"r": map[string]any{"$nin": []int{1}}}, "not (r eq 1)"},
		{"empty in", "", map[string]any{"r": map[string]any{"$in": []int{}}}, "false"},
		{"empty nin", "", map[string]any{"r": map[string]any{"$nin": []int{}}}, "true"},
		{"any scalar", "", map[string]any{"tags": map[string]any{"$any": "x"}}, "tags/any(t: t eq 'x')"},
		{"any slice", "", map[string]any{"tags": map[string]any{"$any": []string{"x", "y"}}}, "tags/any(t: t eq 'x' or t eq 'y')"},
		{"any empty", "", map[string]any{"tags": map[string]any{"$any": []string{}}}, "tags/any()"},
		{"exists", "", map[string]any{"a": map[string]any{"$exists": true}, "b": map[string]any{"$exists": false}}, "a ne null and b eq null"},
		{"raw", "", map[string]any{RawFilterKey: " geo.distance(loc, geography'POINT(0 0)') le 10 ", "a": 1}, "(geo.distance(loc, geography'POINT(0 0)') le 10) and a eq 1"},
		{"raw empty", "", map[string]any{RawFilterKey: "  "}, ""},
		{"special floats", "", map[string]any{"a": math.NaN(), "b": math.Inf(1), "c": math.Inf(-1)}, "a eq NaN and b eq INF and c eq -INF"},
		{"array", "", map[string]any{"a": [2]int{1, 2}}, "(a eq 1 or a eq 2)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildFilter(tc.prefix, tc.filter)
			if err != nil {
				t.Fatalf("BuildFilter: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestBuildFilter_Errors(t *testing.T) {
	cases := map[string]map[string]any{
		"raw not string":   {RawFilterKey: 1},
		"bad key":          {"a b": 1},
		"key injection":    {"a eq 1 or b": 1},
		"empty ops":        {"a": map[string]any{}},
		"unknown op":       {"a": map[string]any{"$regex": "x"}},
		"bad type":         {"a": struct{}{}},
		"bad op value":     {"a": map[string]any{"$gt": struct{}{}}},
		"in not slice":     {"a": map[string]any{"$in": 1}},
		"in bad elem":      {"a": []any{struct{}{}}},
		"exists not bool":  {"a": map[string]any{"$exists": "yes"}},
		"any bad elem":     {"a": map[string]any{"$any": []any{struct{}{}}}},
		"bad json number":  {"a": json.Number("x")},
		"nested map value": {"a": map[string]any{"$eq": map[string]any{}}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildFilter("", f); !errors.Is(err, ErrInvalidFilter) {
				t.Errorf("want ErrInvalidFilter, got %v", err)
			}
		})
	}
}
