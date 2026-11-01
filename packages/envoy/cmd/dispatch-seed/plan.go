package main

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/rank"
)

// seedPlan is every row the run will write, with its key, its uuid and its time, decided before
// anything is written. The text of a row is not here: it is drawn when the row is written, from
// the same source in the same order. The plan is settled first because a mention written into one
// row's text names another row by the id it will have, and a document's text is indexed in the
// same pass that writes it.
type seedPlan struct {
	sizes      sizes
	vocabulary *vocabulary
	random     *rand.Rand

	projects   []projectPlan
	issues     []issuePlan
	documents  []documentPlan
	asks       []askPlan
	comments   []commentPlan
	messages   []messagePlan
	components []componentPlan
	sessions   []string

	// issueAsks and issueComments are the asks and comments a reference can name: the ones an
	// issue owns, whose `dispatch://KEY/ask/<id>` and `dispatch://KEY/comment/<id>` the grammar
	// reads.
	issueAsks     []int
	issueComments []int

	// hub is the issue with HubLinks children and HubLinks blockers, the heavily linked node
	// sjawhar/legion#2177 measured; chainHead the first document of the longest mention chain;
	// anchored the document carrying anchored comments and a table-row anchor; deepest an issue
	// at the deepest parent chain.
	hub       int
	chainHead int
	anchored  int
	deepest   int
}

// projectPlan is one project with its default repository mapping and the second repository the
// issues that carry one of their own use.
type projectPlan struct {
	Key        string
	Name       string
	Repo       string
	OwnRepo    string
	FirstIssue int
	Issues     int
}

// issuePlan is one issue: its key, the project and parent it belongs to, its depth in the parent
// chain (1 for a root), the blockers that link to it, and the primary document it owns.
type issuePlan struct {
	Key       string
	Project   int
	Number    int
	Parent    int // index into issues, or -1
	Depth     int
	Children  int
	Rank      string
	Status    string
	OwnRepo   bool
	CreatedAt time.Time
	Primary   int // index into documents
}

// documentPlan is one `doc` artifact: its uuid, the owner it hangs under (an issue, or a project
// when Issue is -1), its slug and ref_key, how many versions it holds and whether its latest
// version is one of the large ones.
type documentPlan struct {
	ID       string
	Issue    int
	Project  int
	Slug     string
	RefKey   string
	Versions int
	Primary  bool
	Large    bool
	// Chain is the document this one's text mentions, for the chains refs.Closure walks, or -1.
	Chain int
}

// askPlan is one ask. Exactly one of Issue and Document owns it (asks_one_owner); an anchored ask
// is owned by the document it is anchored in, and a block ask names a document and a block of it
// through block_artifact_id.
type askPlan struct {
	ID        string
	Issue     int
	Document  int
	BlockDoc  int
	BlockID   string
	Anchored  bool
	TableRow  bool
	State     string
	CreatedAt time.Time
	Follower  string
}

// commentPlan is one comment. Exactly one of Issue and Document owns it; the rest of the fields
// are the arms of graph_edges a comment can carry: a reply to another comment, an answer to an
// ask, a reply naming a block of another comment, and an anchor into a document.
type commentPlan struct {
	ID         string
	Issue      int
	Document   int
	ReplyTo    int
	Ask        int
	RepliesTo  int // index of the comment whose first block this one replies to, or -1
	Anchored   bool
	TableRow   bool
	Edited     bool
	Blocks     int
	CreatedAt  time.Time
	MentionTo  int // the mention kind this comment writes, or -1 for none
	MentionNth int
}

// messagePlan is one message of an issue thread: a root, or a reply to a root, and for some a
// reply naming a block of another message.
type messagePlan struct {
	ID         string
	Issue      int
	InReplyTo  int
	RepliesTo  int
	Reindexed  bool
	Blocks     int
	CreatedAt  time.Time
	MentionTo  int
	MentionNth int
}

// componentPlan is one component of a project's architecture snapshot, with its parent and the
// component it depends on.
type componentPlan struct {
	Project int
	ID      string
	Parent  string
	Depends string
}

// chainLength is how many documents one mention chain holds. refs.Closure follows mentions for
// eight hops and marks a ninth truncated, so a chain of nine documents is exactly a closure that
// reaches its bound.
const chainLength = 9

// parentChainCap is the deepest parent chain the seed writes. The spec asks for chains of one to
// five levels, well inside api/issue_parent.go's parentDepthCap of 32.
const parentChainCap = 5

// childCap is the most children an ordinary parent takes, from the spec; the one heavily linked
// issue takes HubLinks instead.
const childCap = 30

// newPlan settles every key, uuid and time of the run.
func newPlan(s sizes) *seedPlan {
	p := &seedPlan{sizes: s, vocabulary: newVocabulary(), random: newRandom(), hub: -1}
	p.planProjects()
	p.planSessions()
	p.planIssues()
	p.planDocuments()
	p.planComponents()
	p.planAsks()
	p.planComments()
	p.planMessages()
	p.buildMentionTargets()
	return p
}

func (p *seedPlan) planProjects() {
	s := p.sizes
	p.projects = make([]projectPlan, s.Projects)
	rest := s.Issues - s.BigProjectIssues
	for index := range p.projects {
		key := fmt.Sprintf("SEED%d", index+1)
		issues := s.BigProjectIssues
		if index > 0 {
			issues = share(index-1, s.Projects-1, rest)
		}
		p.projects[index] = projectPlan{
			Key:     key,
			Name:    p.vocabulary.phrase(p.random, 3) + " project",
			Repo:    fmt.Sprintf("seedorg/%s", lower(key)),
			OwnRepo: fmt.Sprintf("seedorg/%s-service", lower(key)),
			Issues:  issues,
		}
	}
}

func (p *seedPlan) planSessions() {
	p.sessions = make([]string, p.sizes.Sessions)
	for index := range p.sessions {
		p.sessions[index] = fmt.Sprintf("seed-session-%05d", index+1)
	}
}

// planIssues lays the issues out project by project, each issue after its parent, so one pass
// over the slice writes rows no foreign key refuses. The first issue of the first project is the
// hub: it takes HubLinks children, which overrides childCap for that one issue.
func (p *seedPlan) planIssues() {
	s := p.sizes
	p.issues = make([]issuePlan, 0, s.Issues)
	statuses := []string{"triage", "backlog", "in_progress", "in_review", "done"}
	for projectIndex := range p.projects {
		project := &p.projects[projectIndex]
		project.FirstIssue = len(p.issues)
		// open holds the issues of this project that may still take a child: inside the depth
		// cap and under childCap.
		var open []int
		previousRank := ""
		for number := 1; number <= project.Issues; number++ {
			index := len(p.issues)
			parent, depth := -1, 1
			switch {
			case projectIndex == 0 && number == 1:
				p.hub = index
			case projectIndex == 0 && number <= 1+s.HubLinks:
				parent, depth = p.hub, 2
			case len(open) > 0 && p.random.Float64() >= 0.25:
				parent = open[p.random.IntN(len(open))]
				depth = p.issues[parent].Depth + 1
			}
			previousRank = rank.Between(previousRank, "")
			p.issues = append(p.issues, issuePlan{
				Key:       fmt.Sprintf("%s-%d", project.Key, number),
				Project:   projectIndex,
				Number:    number,
				Parent:    parent,
				Depth:     depth,
				Rank:      previousRank,
				Status:    statuses[index%len(statuses)],
				OwnRepo:   index%20 == 0,
				CreatedAt: timeAt(index, s.Issues),
				Primary:   index,
			})
			if parent >= 0 {
				p.issues[parent].Children++
				if p.issues[parent].Children >= childCap && parent != p.hub {
					open = removeIndex(open, parent)
				}
			}
			if depth > p.issueDepth(p.deepest) {
				p.deepest = index
			}
			if depth < parentChainCap {
				open = append(open, index)
			}
		}
	}
}

func (p *seedPlan) issueDepth(index int) int {
	if index < 0 || index >= len(p.issues) {
		return 0
	}
	return p.issues[index].Depth
}

// removeIndex drops value from values, keeping the order of the rest, so the choice of a parent
// stays a function of the random source alone.
func removeIndex(values []int, value int) []int {
	for at, held := range values {
		if held == value {
			return append(values[:at:at], values[at+1:]...)
		}
	}
	return values
}

// planDocuments gives every issue its primary document and spreads the rest over the issues and
// the projects. The first documents are the ones with 28 versions and the ones whose latest
// version is large, and the first chains of them are the mention chains the closure walks.
func (p *seedPlan) planDocuments() {
	s := p.sizes
	p.documents = make([]documentPlan, s.Documents)
	for index := range p.documents {
		document := documentPlan{
			ID:       uuidFor("artifact", index),
			Issue:    -1,
			Versions: s.versions(index),
			Large:    index < s.LargeDocuments,
			Chain:    -1,
		}
		switch {
		case index < len(p.issues):
			document.Issue = index
			document.Project = p.issues[index].Project
			document.Slug = "spec"
			document.Primary = true
		case index%3 == 0:
			document.Project = index % len(p.projects)
			document.Slug = fmt.Sprintf("brief-%d", index)
		default:
			document.Issue = index % len(p.issues)
			document.Project = p.issues[document.Issue].Project
			document.Slug = fmt.Sprintf("notes-%d", index)
		}
		if document.Issue >= 0 {
			document.RefKey = p.issues[document.Issue].Key + "/" + document.Slug
		} else {
			document.RefKey = p.projects[document.Project].Key + "/" + document.Slug
		}
		p.documents[index] = document
	}
	// The chains: document j of a chain mentions document j+1, so a closure from the head walks
	// chainLength-1 hops of artifact-to-artifact mentions and the same number of
	// artifact-to-block ones.
	chains := max(1, len(p.documents)/200)
	for chain := range chains {
		for step := range chainLength - 1 {
			index := chain*chainLength + step
			if index+1 >= len(p.documents) {
				break
			}
			p.documents[index].Chain = index + 1
		}
	}
	p.chainHead = 0
	p.anchored = min(len(p.documents)-1, s.LargeDocuments)
}

func (p *seedPlan) planComponents() {
	// Three is the fewest that gives a project a parent, a child and a dependency, which are
	// three arms of graph_edges; a scale that rounded the components down would empty them.
	perProject := max(3, p.sizes.Components/len(p.projects))
	p.components = make([]componentPlan, 0, perProject*len(p.projects))
	for projectIndex := range p.projects {
		for number := range perProject {
			component := componentPlan{Project: projectIndex, ID: fmt.Sprintf("component-%d", number+1)}
			if number > 0 {
				component.Parent = fmt.Sprintf("component-%d", number/3+1)
			}
			if number > 1 {
				component.Depends = fmt.Sprintf("component-%d", number)
			}
			p.components = append(p.components, component)
		}
	}
}

// planAsks spreads the asks over the issues and the documents. Every tenth is anchored in a
// document, every third of those on a table row, which is the subject the anchor reads measure.
func (p *seedPlan) planAsks() {
	s := p.sizes
	p.asks = make([]askPlan, s.Asks)
	states := []string{"open", "answered", "resolved"}
	for index := range p.asks {
		ask := askPlan{
			ID:        uuidFor("ask", index),
			Issue:     -1,
			Document:  -1,
			BlockDoc:  -1,
			State:     states[index%len(states)],
			CreatedAt: timeAt(index, s.Asks),
			Follower:  p.sessions[index%len(p.sessions)],
		}
		switch index % 10 {
		case 0:
			ask.Document = p.documentForAnchor(index)
			ask.Anchored = true
			ask.TableRow = index%30 == 0
		case 1:
			ask.Document = index % len(p.documents)
		case 2:
			ask.Issue = index % len(p.issues)
			// asks_block_id_unique holds one ask to a block, so each block ask takes a
			// document of its own and, once the documents run out, the next paragraph of it.
			ask.BlockDoc = index % len(p.documents)
			ask.BlockID = fmt.Sprintf("b%d", 2+(index/len(p.documents))%paragraphsPerVersion)
		default:
			ask.Issue = index % len(p.issues)
		}
		p.asks[index] = ask
	}
}

// documentForAnchor is the document an anchored ask or comment hangs in. Every anchor of the
// first few falls on p.anchored, so one named document carries anchored comments, an anchored
// ask and a table-row anchor: the subject the anchor reads are measured on.
func (p *seedPlan) documentForAnchor(index int) int {
	if index%7 == 0 {
		return p.anchored
	}
	return index % len(p.documents)
}

// planComments spreads the comments over the issues and the documents, and gives each tenth of
// them one of the arms graph_edges reads a comment through: a reply, an answer to an ask, a reply
// naming a block, and an anchor.
func (p *seedPlan) planComments() {
	s := p.sizes
	p.comments = make([]commentPlan, s.Comments)
	mentioned := 0
	for index := range p.comments {
		comment := commentPlan{
			ID:        uuidFor("comment", index),
			Issue:     -1,
			Document:  -1,
			ReplyTo:   -1,
			Ask:       -1,
			RepliesTo: -1,
			Blocks:    2 + index%3,
			CreatedAt: timeAt(index, s.Comments),
			MentionTo: -1,
			// Every fiftieth comment was edited, so its first indexing is of the body it held
			// before the edit and its second of the body the row holds: the blocks the edit
			// dropped keep their rows with a null position, which is the state the view's
			// `position is not null` filters are written for.
			Edited: index%50 == 49,
		}
		switch index % 10 {
		case 0, 1:
			comment.Document = p.documentForAnchor(index)
			comment.Anchored = true
			comment.TableRow = index%30 == 1
		case 2:
			comment.Document = index % len(p.documents)
		case 3:
			comment.Issue = index % len(p.issues)
			comment.ReplyTo = index - 1
		case 4:
			comment.Issue = index % len(p.issues)
			comment.Ask = index % len(p.asks)
		case 5:
			comment.Issue = index % len(p.issues)
			comment.RepliesTo = index - 1
		default:
			comment.Issue = index % len(p.issues)
		}
		if want := share(index, s.Comments, s.CommentMentions); want > 0 {
			comment.MentionTo = mentioned % len(mentionKinds)
			comment.MentionNth = mentioned
			mentioned++
		}
		p.comments[index] = comment
	}
}

// planMessages lays the issue threads out: a third of the messages are roots and the rest reply
// to the root before them.
func (p *seedPlan) planMessages() {
	s := p.sizes
	p.messages = make([]messagePlan, s.Messages)
	root := -1
	mentioned := 0
	for index := range p.messages {
		message := messagePlan{
			ID:        uuidFor("message", index),
			Issue:     index % len(p.issues),
			InReplyTo: -1,
			RepliesTo: -1,
			Blocks:    2 + index%2,
			CreatedAt: timeAt(index, s.Messages),
			MentionTo: -1,
			// Every fiftieth message is indexed twice, the first time from a body one block
			// longer: a reindex under a parser that reads the same text as fewer blocks, which
			// is what leaves a message block with a null position, the state the view's
			// `position is not null` filter on message_blocks is written for.
			Reindexed: index%50 == 49,
		}
		if index%3 == 0 || root < 0 {
			root = index
		} else {
			message.InReplyTo = root
			message.Issue = p.messages[root].Issue
			if index%9 == 2 {
				message.RepliesTo = root
			}
		}
		if want := share(index, s.Messages, s.MessageMentions); want > 0 {
			message.MentionTo = mentioned % len(mentionKinds)
			message.MentionNth = mentioned
			mentioned++
		}
		p.messages[index] = message
	}
}

func lower(value string) string {
	out := []byte(value)
	for index := range out {
		if out[index] >= 'A' && out[index] <= 'Z' {
			out[index] += 'a' - 'A'
		}
	}
	return string(out)
}
