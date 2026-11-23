package cosmosdb

import (
	"errors"
	"reflect"
	"testing"
)

func TestProp(t *testing.T) {
	cases := map[string]string{
		"embedding":  "c.embedding",
		"_x1":        "c._x1",
		"value":      `c["value"]`,
		"Top":        `c["Top"]`,
		"meta-data":  `c["meta-data"]`,
		`we"ird`:     `c["we\"ird"]`,
		"with space": `c["with space"]`,
	}
	for in, want := range cases {
		if got := prop("c", in); got != want {
			t.Errorf("prop(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuildWhere(t *testing.T) {
	cases := []struct {
		name   string
		filter map[string]any
		where  string
		params []any
	}{
		{"empty", nil, "", nil},
		{"eq", map[string]any{"source": "wiki"}, "c.metadata.source = @f0", []any{"wiki"}},
		{"keyword key", map[string]any{"value": 1}, `c.metadata["value"] = @f0`, []any{1}},
		{"null", map[string]any{"x": nil}, "IS_NULL(c.metadata.x)", nil},
		{"ne null", map[string]any{"x": map[string]any{"$ne": nil}}, "NOT IS_NULL(c.metadata.x)", nil},
		{"slice", map[string]any{"tag": []string{"a", "b"}}, "ARRAY_CONTAINS(@f0, c.metadata.tag)", []any{[]any{"a", "b"}}},
		{"ops", map[string]any{"r": map[string]any{"$gte": 1, "$lt": 5}}, "c.metadata.r >= @f0 AND c.metadata.r < @f1", []any{1, 5}},
		{"all comparisons", map[string]any{"r": map[string]any{"$eq": 1, "$ne": 2, "$gt": 3, "$lte": 4}},
			"c.metadata.r = @f0 AND c.metadata.r > @f1 AND c.metadata.r <= @f2 AND c.metadata.r != @f3", []any{1, 3, 4, 2}},
		{"nin", map[string]any{"r": map[string]any{"$nin": []int{1}}}, "NOT ARRAY_CONTAINS(@f0, c.metadata.r)", []any{[]any{1}}},
		{"in", map[string]any{"r": map[string]any{"$in": [1]int{7}}}, "ARRAY_CONTAINS(@f0, c.metadata.r)", []any{[]any{7}}},
		{"any scalar", map[string]any{"tags": map[string]any{"$any": "x"}}, "ARRAY_CONTAINS(c.metadata.tags, @f0)", []any{"x"}},
		{"any slice", map[string]any{"tags": map[string]any{"$any": []string{"x", "y"}}}, "(ARRAY_CONTAINS(c.metadata.tags, @f0) OR ARRAY_CONTAINS(c.metadata.tags, @f1))", []any{"x", "y"}},
		{"any empty", map[string]any{"tags": map[string]any{"$any": []string{}}}, "false", nil},
		{"exists", map[string]any{"a": map[string]any{"$exists": true}, "b": map[string]any{"$exists": false}}, "IS_DEFINED(c.metadata.a) AND NOT IS_DEFINED(c.metadata.b)", nil},
		{"raw", map[string]any{RawWhereKey: " STARTSWITH(c.id, 'x') ", "a": true}, "(STARTSWITH(c.id, 'x')) AND c.metadata.a = @f0", []any{true}},
		{"raw empty", map[string]any{RawWhereKey: " "}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, params, err := BuildWhere("c.metadata", tc.filter)
			if err != nil {
				t.Fatalf("BuildWhere: %v", err)
			}
			if where != tc.where {
				t.Errorf("where = %q\nwant    %q", where, tc.where)
			}
			if len(params) != len(tc.params) {
				t.Fatalf("params = %+v, want %v", params, tc.params)
			}
			for i, p := range params {
				if p.Name != "@f"+string(rune('0'+i)) {
					t.Errorf("param %d name = %s", i, p.Name)
				}
				if !reflect.DeepEqual(p.Value, tc.params[i]) {
					t.Errorf("param %d = %#v, want %#v", i, p.Value, tc.params[i])
				}
			}
		})
	}
}

func TestBuildWhere_Errors(t *testing.T) {
	cases := map[string]map[string]any{
		"raw not string":  {RawWhereKey: 1},
		"empty key":       {"": 1},
		"empty ops":       {"a": map[string]any{}},
		"unknown op":      {"a": map[string]any{"$regex": "x"}},
		"gt nil":          {"a": map[string]any{"$gt": nil}},
		"gt slice":        {"a": map[string]any{"$gt": []int{1}}},
		"eq map":          {"a": map[string]any{"$eq": map[string]any{}}},
		"in not slice":    {"a": map[string]any{"$in": 1}},
		"exists not bool": {"a": map[string]any{"$exists": 1}},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := BuildWhere("c.metadata", f); !errors.Is(err, ErrInvalidFilter) {
				t.Errorf("want ErrInvalidFilter, got %v", err)
			}
		})
	}
}
