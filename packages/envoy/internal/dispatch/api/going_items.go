package api

import (
	"context"
	"strings"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingItemTextQuery reads one page of hits' texts in one statement, whatever kinds the page
// holds: the hits arrive as two parallel text[] parameters through unnest, so the statement costs
// three parameters and one round trip however many kinds and ids a page names, where a statement
// per hit would cost one round trip each. A kind no arm names, or an id no row holds, answers
// null and is dropped by the final where, so the read refuses nothing.
//
// The five arms mirror the kind arms of search.go's own txt lateral (issue/document/comment/ask/
// message -> table and text column); the two must stay in sync, since this is the text the model
// is given to read for a hit search showed it, and a hit whose body says something other than
// what search ranked is a hit the answer cites for words no reader finds there.
//
// The one arm written differently is the issue's, and deliberately: search's txt lateral reuses
// r.issue_title, the title alone, because a snippet is drawn around the query's own words in it,
// while an answer must be given what meaning search embedded for the issue, which is its title
// and its primary document - issue_embedding_text(i.title, issue_embedding_spec(i.key)), the
// expression embedqueue's issueRows and migration 0140's triggers build an issue's vector from.
// An issue ranked by meaning was ranked on its spec, so showing a model the title alone would
// show it none of what the hit matched.
//
// The ids of four of the kinds are uuids and the issue's is its key, so each wanted id is cast
// once, guarded (store.CastableUUID): an id in no uuid spelling resolves to no row rather than
// answering SQLSTATE 22P02 for the whole page.
//
// The cut is made here, where the row is read, so a whole document version never crosses the wire
// for a hit that shows a paragraph of it: left counts characters in a UTF-8 database, which is the
// cut a page of hits needs, and is how this repository cuts text it is about to show (search.go,
// migrations 0087 and 0100).
var goingItemTextQuery = `with wanted as (
  select w.kind, w.id, ` + store.CastableUUID("w.id") + ` as uuid
    from unnest($1::text[], $2::text[]) as w(kind, id)
)
select w.kind, w.id, left(txt.text, $3::int)
  from wanted w
  left join lateral (
    select case w.kind
      when 'issue' then (select issue_embedding_text(i.title, issue_embedding_spec(i.key)) from issues i where i.key = w.id)
      when 'document' then (select v.markdown from artifact_versions v where v.artifact_id = w.uuid order by v.number desc limit 1)
      when 'comment' then (select c.body from comments c where c.id = w.uuid)
      when 'ask' then (select k.question || ' ' || ask_search_suffix(k.options, k.answer) from asks k where k.id = w.uuid)
      when 'message' then (select m.body from messages m where m.id = w.uuid)
    end as text
  ) txt on true
 where txt.text is not null`

// goingItemText answers each hit's own stored text, cut to runes characters, keyed as
// going.Hit.Key keys it. The statement makes that cut (goingItemTextQuery), so a hit's whole
// stored text never crosses the wire; left counts characters in a UTF-8 database, which is the
// cut text.HeadRunes makes of a whole value in Go. A hit the database holds nothing for is absent
// from the answer, and so is one whose text is only white space. The guard reads the cut text, so
// one hit more is dropped than cutting after the read dropped: a text whose first runes characters
// are white space and whose content comes later was kept, with a body of white space, and is now
// absent, which is a body that showed a reader nothing either way.
func goingItemText(ctx context.Context, pool *store.Pool, hits []going.Hit, runes int) (map[string]string, error) {
	texts := map[string]string{}
	if len(hits) == 0 {
		return texts, nil
	}
	kinds, ids := make([]string, len(hits)), make([]string, len(hits))
	for position, hit := range hits {
		kinds[position], ids[position] = hit.Kind, hit.ID
	}
	rows, err := pool.Query(ctx, goingItemTextQuery, kinds, ids, runes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, value string
		if err := rows.Scan(&kind, &id, &value); err != nil {
			return nil, err
		}
		// White space matches nothing by meaning, which is why the coverage pass leaves such an
		// item out of search at all (embedqueue's nothingToEmbed); an answer has nothing to show
		// for it either.
		if strings.TrimSpace(value) == "" {
			continue
		}
		texts[going.Hit{Kind: kind, ID: id}.Key()] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return texts, nil
}
