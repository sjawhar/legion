package embedqueue

import (
	"context"
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Item names one thing search ranked: its kind, one of coverageSources', and its id within that
// kind, since an id is unique only within one.
type Item struct{ Kind, ID string }

// itemText renders the kind's arm of itemTextQuery: the wanted ids of this kind, each with the
// item's own text, cut to the characters $3 carries. The id and text expressions are
// coverageSources', so the text an answer is given is the text search ranked, and the join
// against wanted is what keeps the arm to the ids asked for rather than to the kind's whole
// table. The cut is made here, where the row is read, so a whole document version never crosses
// the wire for a hit that shows a paragraph of it: left counts characters in a UTF-8 database,
// which is the cut a page of hits needs, and is how this repository cuts text it is about to
// show (api/search.go, migrations 0087 and 0100).
func (s coverageSource) itemText() string {
	where := ""
	if s.where != "" {
		where = " where " + s.where
	}
	return fmt.Sprintf(`select w.kind, w.id, left(s.text, $3::int)
		from (select %s as id, %s as text from %s%s) s
		join wanted w on w.kind = '%s' and w.id = s.id`, s.id, s.text, s.from, where, s.kind)
}

// itemTextQuery reads one page of items' texts in one statement, whatever kinds the page holds:
// the wanted ids arrive as two parallel text[] parameters through unnest, so the statement costs
// three parameters and one round trip however many kinds and ids a page names, where a statement
// per item would cost one round trip each. A kind coverageSources does not list has no arm, so
// an item of one matches nothing and is absent from the answer: the read refuses nothing.
var itemTextQuery = func() string {
	arms := make([]string, 0, len(coverageSources))
	for _, source := range coverageSources {
		arms = append(arms, source.itemText())
	}
	return `with wanted as (select kind, id from unnest($1::text[], $2::text[]) as w(kind, id))
` + strings.Join(arms, "\nunion all\n")
}()

// ItemText answers each named item's own stored text, cut to runes characters, keyed
// "<kind>:<id>". The statement makes that cut (itemText), so an item's whole stored text never
// crosses the wire; left counts characters in a UTF-8 database, which is the cut text.HeadRunes
// made of the whole value here before. An item it finds nothing for is absent from the answer,
// and an item whose text is only white space is too. The guard reads the cut text, so one item
// more is dropped than cutting after the read dropped: a text whose first runes characters are
// white space and whose content comes later was kept, with a body of white space, and is now
// absent, which is a body that showed a reader nothing either way.
func ItemText(ctx context.Context, pool *store.Pool, items []Item, runes int) (map[string]string, error) {
	texts := map[string]string{}
	if len(items) == 0 {
		return texts, nil
	}
	kinds, ids := make([]string, len(items)), make([]string, len(items))
	for position, item := range items {
		kinds[position], ids[position] = item.Kind, item.ID
	}
	rows, err := pool.Query(ctx, itemTextQuery, kinds, ids, runes)
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
		// item out (nothingToEmbed); an answer has nothing to show for it either.
		if strings.TrimSpace(value) == "" {
			continue
		}
		texts[kind+":"+id] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return texts, nil
}
