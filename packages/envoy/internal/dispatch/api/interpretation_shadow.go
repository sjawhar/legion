package api

import (
	"context"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type interpretationRelation struct {
	document string
	blockID  string
	decision string
}

func (r interpretationRelation) signature() string {
	return refs.BlockRef(r.document, r.blockID) + " interprets " + r.decision
}

type interpretationComparison struct {
	sourceCount int
	nodeCount   int
	missing     []string
	extra       []string
}

func (s *server) compareInterpretations(ctx context.Context, key string) (interpretationComparison, error) {
	var result interpretationComparison
	rows, err := s.deps.Store.Pool.Query(ctx, `select id::text,slug,is_primary,project_key from artifacts where issue_key=$1 and kind='doc' order by id`, key)
	if err != nil {
		return result, err
	}
	var documents []model.Artifact
	for rows.Next() {
		document := model.Artifact{IssueKey: &key, Kind: "doc"}
		if err := rows.Scan(&document.ID, &document.Slug, &document.Primary, &document.Project); err != nil {
			rows.Close()
			return result, err
		}
		documents = append(documents, document)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return result, err
	}

	source := make(map[interpretationRelation]bool)
	for _, document := range documents {
		_, blocks, err := s.deps.Docs.TextWithBlocks(ctx, document.ID)
		if err != nil {
			return result, err
		}
		reference := artifactReference(document)
		for _, block := range blocks {
			if block.Type != "interpretation" {
				continue
			}
			for _, value := range strings.Fields(blockAttribute(block, "interprets")) {
				if pointer, valid := text.CanonicalBlockRef(value); valid {
					pointer.Pin = 0
					parent, _ := text.FormatBlockRef(pointer)
					source[interpretationRelation{reference, block.ID, parent}] = true
				}
			}
		}
	}

	rows, err = s.deps.Store.Pool.Query(ctx, interpretationEdgesQuery, key)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	graph := make(map[interpretationRelation]bool)
	for rows.Next() {
		var documentID, blockID, parentID, slug string
		var primary bool
		if err := rows.Scan(&documentID, &blockID, &parentID, &slug, &primary); err != nil {
			return result, err
		}
		reference := refs.ArtifactRef(&key, "", nil, slug, "doc", primary)
		graph[interpretationRelation{reference, blockID, refs.BlockRef(reference, parentID)}] = true
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	result.sourceCount, result.nodeCount = len(source), len(graph)
	for relation := range source {
		if !graph[relation] {
			result.missing = append(result.missing, relation.signature())
		}
	}
	for relation := range graph {
		if !source[relation] {
			result.extra = append(result.extra, relation.signature())
		}
	}
	return result, nil
}
