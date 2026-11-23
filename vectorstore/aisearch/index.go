package aisearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Edm types usable for metadata fields in CreateIndex.
const (
	EdmString         = "Edm.String"
	EdmInt32          = "Edm.Int32"
	EdmInt64          = "Edm.Int64"
	EdmDouble         = "Edm.Double"
	EdmBoolean        = "Edm.Boolean"
	EdmDateTimeOffset = "Edm.DateTimeOffset"
	EdmStringList     = "Collection(Edm.String)"
)

// Names of the vector-search configuration written by CreateIndex.
const (
	vectorProfileName   = "golly-vector-profile"
	vectorAlgorithmName = "golly-hnsw"
)

// MetadataField declares one metadata attribute for CreateIndex. Azure AI
// Search indexes have a fixed schema: every metadata key you Upsert must be
// declared up front.
type MetadataField struct {
	// Name is the metadata key (a valid OData identifier).
	Name string
	// Type is the Edm type (EdmString, EdmInt64, EdmStringList, ...).
	// Defaults to EdmString.
	Type string
	// Filterable marks the field usable in Query.Filter. Defaults to true
	// when no attribute flag is set at all.
	Filterable bool
	// Facetable / Sortable / Searchable map to the matching index attributes.
	Facetable  bool
	Sortable   bool
	Searchable bool
}

// CreateIndex is a bootstrap helper: it issues PUT /indexes/{index}
// (create-or-update) with a schema matching the Store's field names:
//
//	id        Edm.String, key, filterable
//	content   Edm.String, searchable (BM25 for hybrid queries)
//	embedding Collection(Edm.Single), dimensions=Options.Dimension,
//	          vectorSearchProfile -> HNSW with Options.Metric
//	metadata  Edm.ComplexType with the supplied sub-fields
//	          (or top-level fields when FlattenMetadata is set)
//
// Callers who manage their index schema out of band (Bicep, Terraform, the
// portal, ...) should skip it. Requires an admin key or the "Search Service
// Contributor" role.
func (s *Store) CreateIndex(ctx context.Context, metadata ...MetadataField) error {
	if s.closed.Load() {
		return errClosed
	}
	body, err := s.indexDefinition(metadata)
	if err != nil {
		return err
	}
	if err := s.do(ctx, http.MethodPut, s.indexPath(), body, nil,
		http.StatusOK, http.StatusCreated, http.StatusNoContent); err != nil {
		return fmt.Errorf("vectorstore/aisearch: create index %q: %w", s.index, err)
	}
	return nil
}

// DeleteIndex removes the whole index (DELETE /indexes/{index}). A missing
// index is not an error.
func (s *Store) DeleteIndex(ctx context.Context) error {
	if s.closed.Load() {
		return errClosed
	}
	if err := s.do(ctx, http.MethodDelete, s.indexPath(), nil, nil,
		http.StatusOK, http.StatusNoContent, http.StatusNotFound); err != nil {
		return fmt.Errorf("vectorstore/aisearch: delete index %q: %w", s.index, err)
	}
	return nil
}

// indexDefinition builds the CreateIndex request body.
func (s *Store) indexDefinition(metadata []MetadataField) (map[string]any, error) {
	if s.dimension <= 0 {
		return nil, errors.New("vectorstore/aisearch: CreateIndex requires Options.Dimension > 0")
	}
	fields := []map[string]any{
		{"name": s.idField, "type": EdmString, "key": true, "filterable": true},
		{"name": s.contentField, "type": EdmString, "searchable": true, "filterable": false},
		{
			"name":                s.vectorField,
			"type":                "Collection(Edm.Single)",
			"searchable":          true,
			"retrievable":         false,
			"dimensions":          s.dimension,
			"vectorSearchProfile": vectorProfileName,
		},
	}
	reserved := map[string]bool{s.idField: true, s.contentField: true, s.vectorField: true}
	sub := make([]map[string]any, 0, len(metadata))
	for _, m := range metadata {
		if !fieldPathRE.MatchString(m.Name) || strings.Contains(m.Name, "/") {
			return nil, fmt.Errorf("vectorstore/aisearch: invalid metadata field name %q", m.Name)
		}
		if s.flatten && reserved[m.Name] {
			return nil, fmt.Errorf("vectorstore/aisearch: metadata field %q collides with a reserved field", m.Name)
		}
		typ := m.Type
		if typ == "" {
			typ = EdmString
		}
		filterable := m.Filterable
		if !m.Filterable && !m.Facetable && !m.Sortable && !m.Searchable {
			filterable = true
		}
		f := map[string]any{"name": m.Name, "type": typ, "filterable": filterable}
		if m.Facetable {
			f["facetable"] = true
		}
		if m.Sortable {
			f["sortable"] = true
		}
		if m.Searchable {
			f["searchable"] = true
		}
		sub = append(sub, f)
	}
	if s.flatten {
		fields = append(fields, sub...)
	} else if len(sub) > 0 {
		fields = append(fields, map[string]any{
			"name":   s.metadataField,
			"type":   "Edm.ComplexType",
			"fields": sub,
		})
	}
	return map[string]any{
		"name":   s.index,
		"fields": fields,
		"vectorSearch": map[string]any{
			"algorithms": []map[string]any{{
				"name":           vectorAlgorithmName,
				"kind":           "hnsw",
				"hnswParameters": map[string]any{"metric": string(s.metric)},
			}},
			"profiles": []map[string]any{{
				"name":      vectorProfileName,
				"algorithm": vectorAlgorithmName,
			}},
		},
	}, nil
}
