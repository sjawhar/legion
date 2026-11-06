// Command browsertype types into Dispatch documents as browsers do, so a load test can run
// concurrent browser edits beside the agents it drives over HTTP. It connects peers to each
// document's websocket room as a person of its own, named in the server's identity header (a
// server run with DISPATCH_IDENTITY=header:<name> and DISPATCH_IDENTITY_HEADER_TRUSTED=1, as a load
// test's stack is: the room admits a person alone, never an agent token), and each peer, once it
// holds the room's document, sends keystroke updates at a set rate: most type one character into an
// existing paragraph's text, some replace a short range (an insert and a delete in one
// transaction), some delete a short range. It writes only text inside existing paragraphs, never
// block structure, so every document stays valid.
//
// At the end it prints one line per peer, and exits 1 when a peer failed to connect or its
// connection ended with an error before the run did, 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// fragmentName is the Y.XmlFragment the docs package keeps a document's ProseMirror tree in (its
// own fragmentName, unexported).
const fragmentName = "prosemirror"

type config struct {
	baseURL        *url.URL
	identityHeader string
	person         string
	artifacts      []string
	peers          int
	rate           float64
	duration       time.Duration
	connectTimeout time.Duration
	seed           uint64
	protect        []string
}

// result is what one peer did: the updates it sent, the keystrokes it skipped because the document
// held no paragraph text it may edit then, and the error that ended it early, if any.
type result struct {
	artifact string
	peer     int
	sent     int
	skipped  int
	err      error
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cfg, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "browsertype: %v\n", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.duration)
	defer cancel()

	results := make([]result, len(cfg.artifacts)*cfg.peers)
	var wg sync.WaitGroup
	for a, artifact := range cfg.artifacts {
		for p := range cfg.peers {
			index := a*cfg.peers + p
			results[index] = result{artifact: artifact, peer: p}
			wg.Go(func() {
				random := rand.New(rand.NewPCG(cfg.seed, uint64(index)))
				// Each peer is a person of its own, as a browser is: <person>-<peer>.
				header := http.Header{}
				header.Set(cfg.identityHeader, fmt.Sprintf("%s-%d", cfg.person, p))
				typePeer(ctx, cfg, roomURL(cfg.baseURL, artifact), header, random, &results[index])
			})
		}
	}
	wg.Wait()

	failed := false
	for _, r := range results {
		failures, message := 0, ""
		if r.err != nil {
			failures, message, failed = 1, r.err.Error(), true
		}
		fmt.Printf("artifact=%s peer=%d sent=%d skipped=%d errors=%d error=%q\n", r.artifact, r.peer, r.sent, r.skipped, failures, message)
	}
	if failed {
		return 1
	}
	return 0
}

func parseFlags(args []string) (config, error) {
	flags := flag.NewFlagSet("browsertype", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: browsertype -url URL (-artifact ID ... | -artifacts-file FILE) [flags]\n\n"+
			"Connects -peers browser peers to each document's websocket room and types into its paragraphs at\n"+
			"-rate keystrokes per second per peer until -duration ends or SIGINT/SIGTERM: about 80%% type one\n"+
			"letter, 10%% replace a short range in one transaction, 10%% delete a short range. A paragraph whose\n"+
			"text holds a -protect value is never edited. Prints one line per peer (artifact, peer, sent,\n"+
			"skipped, errors, error); exits 1 when a peer failed to connect or ended early with an error, 2 on\n"+
			"a usage error.\n\nFlags:\n")
		flags.PrintDefaults()
	}
	var cfg config
	var rawURL, artifactsFile string
	var artifacts, protect stringList
	flags.StringVar(&rawURL, "url", "", "Dispatch base URL, http(s)://host:port; peers connect to <url>/ws/doc/<artifact>")
	flags.StringVar(&cfg.identityHeader, "identity-header", "X-Dispatch-User", "the header the server's DISPATCH_IDENTITY=header:<name> reads a person from")
	flags.StringVar(&cfg.person, "person", "browsertype", "login prefix of the people the peers are: peer n of every document is <person>-<n>")
	flags.Var(&artifacts, "artifact", "document artifact id to type into (repeatable)")
	flags.StringVar(&artifactsFile, "artifacts-file", "", "file of document artifact ids, one per line (blank lines and lines starting with # skipped); adds to -artifact")
	flags.IntVar(&cfg.peers, "peers", 2, "peers per document")
	flags.Float64Var(&cfg.rate, "rate", 5, "keystroke updates per second, per peer")
	flags.DurationVar(&cfg.duration, "duration", time.Minute, "how long the peers type")
	flags.DurationVar(&cfg.connectTimeout, "connect-timeout", 30*time.Second, "how long a peer may take to connect and receive the room's document")
	flags.Uint64Var(&cfg.seed, "seed", 1, "seed of the peers' random choices (each peer draws from its own stream of it)")
	flags.Var(&protect, "protect", "text marking a paragraph the peers never edit: one whose text holds it, anywhere in the document (repeatable)")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if flags.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments %q", flags.Args())
	}

	base, err := url.Parse(rawURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return config{}, fmt.Errorf("-url must be http(s)://host:port, got %q", rawURL)
	}
	cfg.baseURL = base
	if strings.TrimSpace(cfg.identityHeader) == "" {
		return config{}, errors.New("-identity-header must name a header")
	}
	if strings.TrimSpace(cfg.person) == "" {
		return config{}, errors.New("-person must name a login prefix")
	}
	cfg.artifacts = artifacts
	for _, text := range protect {
		if text == "" {
			return config{}, errors.New("-protect must name some text")
		}
	}
	cfg.protect = protect
	if artifactsFile != "" {
		content, err := os.ReadFile(artifactsFile)
		if err != nil {
			return config{}, fmt.Errorf("read -artifacts-file: %w", err)
		}
		for line := range strings.Lines(string(content)) {
			if id := strings.TrimSpace(line); id != "" && !strings.HasPrefix(id, "#") {
				cfg.artifacts = append(cfg.artifacts, id)
			}
		}
	}
	if len(cfg.artifacts) == 0 {
		return config{}, errors.New("name at least one document with -artifact or -artifacts-file")
	}
	if cfg.peers < 1 {
		return config{}, fmt.Errorf("-peers must be at least 1, got %d", cfg.peers)
	}
	if !(cfg.rate > 0) {
		return config{}, fmt.Errorf("-rate must be above 0, got %v", cfg.rate)
	}
	if cfg.duration <= 0 || cfg.connectTimeout <= 0 {
		return config{}, errors.New("-duration and -connect-timeout must be above 0")
	}
	return cfg, nil
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// roomURL is artifact's document websocket at base, at the schema version the dashboard presents,
// so the room takes the peer's edits rather than holding it read-only.
func roomURL(base *url.URL, artifact string) string {
	room := *base
	room.Scheme = "ws"
	if base.Scheme == "https" {
		room.Scheme = "wss"
	}
	room.Path = strings.TrimSuffix(base.Path, "/") + "/ws/doc/" + url.PathEscape(artifact)
	room.RawQuery = url.Values{"schema_version": {strconv.Itoa(pmdoc.SchemaVersion())}}.Encode()
	return room.String()
}

// typePeer connects one peer, waits until it holds the room's document, and types until ctx ends,
// recording what it did in r.
func typePeer(ctx context.Context, cfg config, room string, header http.Header, random *rand.Rand, r *result) {
	connectCtx, cancelConnect := context.WithTimeout(ctx, cfg.connectTimeout)
	defer cancelConnect()
	peer, err := docstest.DialContext(connectCtx, room, header, r.artifact, crdt.New())
	if err != nil {
		r.err = err
		return
	}
	defer peer.Close()
	if err := peer.AskForDocument(); err != nil {
		r.err = fmt.Errorf("ask for the room's document: %w", err)
		return
	}
	select {
	case <-peer.Answers:
	case <-peer.Ended:
		r.err = errors.New("the connection ended before the room sent its document")
		return
	case <-connectCtx.Done():
		if ctx.Err() == nil {
			r.err = fmt.Errorf("the room sent no document within %s", cfg.connectTimeout)
		}
		return
	}
	fragment := peer.Doc.GetXmlFragment(fragmentName)
	var texts int
	peer.Doc.Transact(func(*crdt.Transaction) { texts = len(editableTexts(fragment, cfg.protect)) })
	if texts == 0 {
		r.err = errors.New("the document holds no paragraph text the peer may type into")
		return
	}

	ticker := time.NewTicker(time.Duration(float64(time.Second) / cfg.rate))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-peer.Ended:
			if ctx.Err() == nil {
				r.err = errors.New("the connection ended before the run did")
			}
			return
		case <-ticker.C:
		}
		update := docstest.Transact(peer.Doc, func(txn *crdt.Transaction) { keystroke(txn, fragment, random, cfg.protect) })
		if update == nil {
			r.skipped++
			continue
		}
		if err := peer.Write(ygsync.EncodeUpdate(update)); err != nil {
			if ctx.Err() == nil {
				r.err = fmt.Errorf("send an update: %w", err)
			}
			return
		}
		r.sent++
	}
}

// keystroke makes one edit to a paragraph's text chosen at random, reading the document inside the
// transaction so the edit lands on what the peer holds now, room edits included. A paragraph whose
// text holds one of protect is never chosen: a load generator's own writes quote it. It changes
// nothing when the document holds no paragraph text it may edit, and never deletes a text's last
// character.
func keystroke(txn *crdt.Transaction, fragment *crdt.YXmlFragment, random *rand.Rand, protect []string) {
	texts := editableTexts(fragment, protect)
	if len(texts) == 0 {
		return
	}
	text := texts[random.IntN(len(texts))]
	length := text.Len()
	switch roll := random.IntN(10); {
	case roll == 0 && length >= 2:
		// Replace a selection: delete one to three characters and type a short word there.
		at := random.IntN(length)
		removed := 1 + random.IntN(min(3, length-at, length-1))
		text.Delete(txn, at, removed)
		text.Insert(txn, at, word(random, 1+random.IntN(4)), nil)
	case roll == 1 && length >= 2:
		at := random.IntN(length)
		text.Delete(txn, at, 1+random.IntN(min(5, length-at, length-1)))
	default:
		at := random.IntN(length + 1)
		letter := word(random, 1)
		// A space only inside the text, so no paragraph gains leading or trailing blanks.
		if at > 0 && at < length && random.IntN(6) == 0 {
			letter = " "
		}
		text.Insert(txn, at, letter, nil)
	}
}

func word(random *rand.Rand, n int) string {
	letters := make([]byte, n)
	for i := range letters {
		letters[i] = byte('a' + random.IntN(26))
	}
	return string(letters)
}

// paragraphTexts is every text directly inside a paragraph element anywhere in fragment's tree
// (top level, lists, quotes, cells), in document order. Callers hold the document's transaction.
func paragraphTexts(fragment *crdt.YXmlFragment) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	for _, paragraph := range paragraphs(fragment) {
		texts = append(texts, paragraph...)
	}
	return texts
}

// editableTexts is paragraphTexts without the texts of a paragraph whose whole text, read now,
// holds one of protect. Callers hold the document's transaction.
func editableTexts(fragment *crdt.YXmlFragment, protect []string) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	for _, paragraph := range paragraphs(fragment) {
		if !holdsAny(paragraph, protect) {
			texts = append(texts, paragraph...)
		}
	}
	return texts
}

// paragraphs is the texts of each paragraph element anywhere in fragment's tree, one slice per
// paragraph, in document order.
func paragraphs(fragment *crdt.YXmlFragment) [][]*crdt.YXmlText {
	var found [][]*crdt.YXmlText
	var walk func(*crdt.YXmlFragment)
	walk = func(parent *crdt.YXmlFragment) {
		for _, child := range parent.Children() {
			element, ok := child.(*crdt.YXmlElement)
			if !ok {
				continue
			}
			if element.NodeName != "paragraph" {
				walk(&element.YXmlFragment)
				continue
			}
			var texts []*crdt.YXmlText
			for _, inner := range element.Children() {
				if text, ok := inner.(*crdt.YXmlText); ok {
					texts = append(texts, text)
				}
			}
			found = append(found, texts)
		}
	}
	walk(fragment)
	return found
}

// holdsAny reports whether the paragraph made of texts holds one of protect in its plain text,
// read inside the caller's transaction (YText.ToString would wait on the lock that holds).
func holdsAny(texts []*crdt.YXmlText, protect []string) bool {
	if len(protect) == 0 {
		return false
	}
	var plain strings.Builder
	for _, text := range texts {
		plain.WriteString(pmdoc.TextInTransaction(text))
	}
	for _, marker := range protect {
		if strings.Contains(plain.String(), marker) {
			return true
		}
	}
	return false
}
