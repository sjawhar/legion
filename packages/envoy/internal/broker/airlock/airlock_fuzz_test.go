package airlock

// FuzzAirlock drives Validate, Decide and Rewrite against the Lean model of exactly those
// functions (lean/Airlock/Model.lean, built as the airlock-model program AIRLOCK_MODEL
// names). Every generated case goes to both sides and the two answers must be the same
// value, so a change on one side that is not made on the other is a test failure here
// rather than a surprise in production. This file is in package airlock, not package
// airlock_test: the generator and the wire shapes sit beside the code they mirror, and
// the comparison reaches the three functions with nothing in between.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// ---- The Lean model as a long-lived subprocess ---------------------------------------

// modelBuildPath is where lake build writes the model, relative to this package directory,
// which is the working directory of a test in this package. It is what AIRLOCK_MODEL is
// pointed at, by hand and by scripts/airlock-mutants.sh, and what the skip below names.
const modelBuildPath = "lean/.lake/build/bin/airlock-model"

// modelPath names the built airlock-model binary: AIRLOCK_MODEL, and nothing else. With
// that variable unset, or naming no file, it answers "" and FuzzAirlock skips, so a plain
// `go test ./...` of this module needs no Lean toolchain and no built model, and a run
// that does compare the two sides says which model it compared against rather than
// picking up whatever a past lake build left in the tree.
func modelPath() string {
	bin := os.Getenv("AIRLOCK_MODEL")
	if bin == "" {
		return ""
	}
	if fi, err := os.Stat(bin); err != nil || fi.IsDir() {
		return ""
	}
	return bin
}

// model is one airlock-model process, fed one JSON case per line and read one JSON answer
// per line. Starting a process per case costs milliseconds each and would dominate the run,
// so the process is started once and kept alive. Go runs each fuzz worker as its own
// process of this test binary, so one process per binary is one model per worker; the mutex
// keeps the line protocol a single conversation when -parallel drives one worker's target
// from several goroutines.
type model struct {
	mu  sync.Mutex
	in  *bufio.Writer
	out *bufio.Reader
}

var (
	modelOnce sync.Once
	theModel  *model
	modelErr  error
)

func leanModel(t testing.TB) *model {
	modelOnce.Do(func() {
		cmd := exec.Command(modelPath())
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			modelErr = err
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			modelErr = err
			return
		}
		if modelErr = cmd.Start(); modelErr != nil {
			return
		}
		theModel = &model{in: bufio.NewWriter(stdin), out: bufio.NewReaderSize(stdout, 1<<20)}
	})
	if modelErr != nil {
		t.Fatalf("start lean model %s: %v", modelPath(), modelErr)
	}
	return theModel
}

// wireCase is one line of the model's input protocol. The rules, the grant and the request
// are the package's own types, marshalled by encoding/json: the model's derived FromJson
// ignores a field's default, so every field has to be on the wire, which marshalling the
// real types gives for free and a hand-built JSON object would not.
type wireCase struct {
	Rules   Rules    `json:"rules"`
	Grant   Grant    `json:"grant"`
	Value   string   `json:"value"`
	Request *Request `json:"request"`
}

// answer is one line of the model's output protocol, and the same shape Go's side is read
// into, so the two are compared as values. Error carries the model's parse refusal.
type answer struct {
	Valid   bool     `json:"valid"`
	Allow   bool     `json:"allow"`
	Reason  string   `json:"reason"`
	Rewrite *Request `json:"rewrite"`
	Error   string   `json:"error,omitempty"`
}

func (m *model) ask(t testing.TB, line []byte) answer {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.in.Write(line)
	m.in.WriteByte('\n')
	if err := m.in.Flush(); err != nil {
		t.Fatalf("write to model: %v", err)
	}
	resp, err := m.out.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read from model: %v", err)
	}
	var a answer
	if err := json.Unmarshal(resp, &a); err != nil {
		t.Fatalf("model answer %q: %v", resp, err)
	}
	if a.Error != "" {
		t.Fatalf("model could not parse case %s: %s", line, a.Error)
	}
	return a
}

func goAnswer(c wireCase) answer {
	allow, reason := Decide(c.Rules, c.Grant, c.Value, c.Request)
	return answer{
		Valid:   Validate(c.Rules) == nil,
		Allow:   allow,
		Reason:  reason,
		Rewrite: Rewrite(c.Rules, c.Grant, c.Value, c.Request),
	}
}

// ---- Case generator: a byte string drives choices among tricky pools ------------------

type src struct{ b []byte }

func (s *src) u8() byte {
	if len(s.b) == 0 {
		return 0
	}
	c := s.b[0]
	s.b = s.b[1:]
	return c
}

// raw takes up to 15 arbitrary bytes (may be invalid UTF-8).
func (s *src) raw() string {
	n := int(s.u8() % 16)
	if n > len(s.b) {
		n = len(s.b)
	}
	x := string(s.b[:n])
	s.b = s.b[n:]
	return x
}

func (s *src) text() string { return strings.ToValidUTF8(s.raw(), "") }

// pick returns a pool entry, or (1 in 8) fresh valid-UTF-8 text.
func (s *src) pick(pool []string) string {
	i := int(s.u8())
	if i%8 == 7 {
		return s.text()
	}
	return pool[(i/8)%len(pool)] // i%8 picked the escape; i/8 reaches every entry
}

// port returns a portPool entry, or (1 in 8) a free 16-bit draw, so the comparison covers
// every port and not only the ones named here.
func (s *src) port() uint16 {
	i := int(s.u8())
	if i%8 == 7 {
		return uint16(s.u8())<<8 | uint16(s.u8())
	}
	return portPool[(i/8)%len(portPool)] // i%8 picked the escape; i/8 reaches every entry
}

var (
	hostPool   = []string{"api.github.com", "*.github.com", "github.com", "*.com", "*.", "API.GitHub.com", "*.GitHub.COM", "uploads.github.com", "a.b.github.com", ".github.com", "githu\u212a.com", "githuk.com", "\u0130.github.com", ""}
	namePool   = []string{"gh", "aws", "GH", ""}
	hdrPool    = []string{"authorization", "Authorization", "AUTHORIZATION", "x-api-key", "X-Api-Key", "x-api-\u212aey", "proxy-authorization", "cookie"}
	methodPool = []string{"GET", "POST", "get"}
	// overridePool is the three headers check refuses under an entry with an allow list,
	// each in a case the ASCII fold has to work on, with two near misses: a longer name
	// that only starts like one of them, and the KELVIN SIGN spelling that the fold here
	// leaves alone where strings.EqualFold would not.
	overridePool = []string{"x-http-method-override", "X-HTTP-Method-Override", "x-http-method", "X-Http-Method", "x-method-override", "X-METHOD-OVERRIDE", "x-http-method-overridden", "x-http-met\u212aod"}
	// pathPool holds paths with no dot segment, paths that carry one (so check's "path has a
	// dot segment" refusal is reached), and two near misses - "..foo" and "..." are segments
	// no reader resolves - so the comparison covers where each side draws that line.
	pathPool = []string{"/", "/repos/x", "/repos", "/admin", "", "/v1/../admin", "/a/./b", "/..", "/.", "/v1/things/../../admin", "/v1/..foo", "/v1/.../x"}
	// portPool is the CONNECT port: 443 five times in eight, so a case reaches the refusals
	// below the port, and otherwise a port the rules say nothing about.
	portPool   = []uint16{443, 443, 443, 443, 443, 80, 8443, 0}
	prefixPool = []string{"/", "/repos", "", "/r", "/admin/"}
	phPool     = []string{"PH_gh_1", "ph", "", "a", "=", "user"}
	valuePool  = []string{"REAL", "ghp_secret", "", "PH_gh_1", "a:b", "\u00e9"}
	// kindPool holds the three kinds Validate takes, a wrong one, and a right one in the
	// wrong case, so validate's kind refusal is reached from both sides of the comparison.
	kindPool   = []string{"", "aws", "azure", "gcp", "AWS"}
	judgePool  = []uint32{0, 300, 900, 901, 65535}
	piecePool  = []string{"user", "token ", " ", "\t", "x"}
	schemePool = []string{"Basic ", "basic ", "BASIC ", "Basic  ", "Basic\t", "Basi "}
	tailPool   = []string{"\v", " ", "\t", "\n", "\u00a0", "\u0085"}
)

func (s *src) list(n int, f func() string) []string {
	out := []string{}
	for range n {
		out = append(out, f())
	}
	return out
}

// piece builds a fragment that often holds the placeholder (so the swap has work to do) or
// the value (so check's "request already carries the value" refusal is reached).
func (s *src) piece(g Grant, value string, allowRaw bool) string {
	switch s.u8() % 8 {
	case 0, 1:
		return g.Placeholder
	case 2, 3:
		return value
	case 4:
		return ":"
	case 5:
		return "Bearer "
	case 6:
		if allowRaw {
			return s.raw() // may be invalid UTF-8: only used inside Basic payloads
		}
		return s.text()
	default:
		return s.pick(piecePool)
	}
}

func (s *src) compose(g Grant, value string, allowRaw bool) string {
	var b strings.Builder
	for n := 1 + int(s.u8()%3); n > 0; n-- {
		b.WriteString(s.piece(g, value, allowRaw))
	}
	return b.String()
}

// headerValue builds one header value: two in three plain, one in three a Basic credential
// whose payload is composed the same way. Both shapes carry the placeholder and the value
// often, so the swap and the "already carries the value" refusal are each reached directly
// and through a decoded Basic credential.
func (s *src) headerValue(g Grant, value string) string {
	k := s.u8()
	if k%3 != 0 {
		return s.compose(g, value, false)
	}
	scheme := s.pick(schemePool)
	enc := base64.StdEncoding.EncodeToString([]byte(s.compose(g, value, true)))
	switch s.u8() % 8 {
	case 1:
		enc = strings.TrimRight(enc, "=")
	case 2:
		if len(enc) > 4 {
			enc = enc[:4] + "\r\n" + enc[4:]
		}
	case 3:
		enc += s.pick(tailPool)
	case 4:
		if i := strings.IndexByte(enc, '='); i > 0 {
			b := []byte(enc)
			b[i-1] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"[s.u8()%64]
			enc = string(b)
		}
	case 5:
		enc += s.text()
	}
	return scheme + enc
}

func genCase(data []byte) wireCase {
	s := &src{b: data}
	rules := Rules{Secrets: []SecretRule{}}
	for n := int(s.u8() % 4); n > 0; n-- {
		sr := SecretRule{Name: s.pick(namePool), Kind: s.pick(kindPool)}
		// Hosts is never nil: the model's hosts is a List String, and JSON null is no
		// list, so a nil slice would be a case the model refuses to parse rather than a
		// case about an entry with no host. The empty list is that case, and list gives it.
		sr.Hosts = s.list(int(s.u8()%3), func() string { return s.pick(hostPool) })
		switch s.u8() % 3 { // nil: the default ["authorization"]; empty: no header; or names
		case 1:
			sr.Headers = []string{}
		case 2:
			sr.Headers = s.list(1+int(s.u8()%3), func() string { return s.pick(hdrPool) })
		}
		switch s.u8() % 3 { // nil: any method and path; empty: nothing allowed; or rows
		case 1:
			sr.Allow = []Allow{}
		case 2:
			sr.Allow = []Allow{}
			for m := 1 + int(s.u8()%3); m > 0; m-- {
				sr.Allow = append(sr.Allow, Allow{Method: s.pick(methodPool), PathPrefix: s.pick(prefixPool)})
			}
		}
		switch s.u8() % 6 {
		case 1, 2:
			j := judgePool[int(s.u8())%len(judgePool)]
			sr.Judge = &j
		case 3:
			j := uint32(s.u8())<<8 | uint32(s.u8())
			sr.Judge = &j
		}
		rules.Secrets = append(rules.Secrets, sr)
	}
	g := Grant{Secret: s.pick(namePool), Placeholder: s.pick(phPool)}
	value := s.pick(valuePool)
	r := &Request{ConnectHost: s.pick(hostPool), ConnectPort: s.port(), Method: s.pick(methodPool), Path: s.pick(pathPool), Headers: [][2]string{}}
	r.SNI, r.HostHeader = r.ConnectHost, r.ConnectHost
	if s.u8()%4 == 0 {
		r.SNI = s.pick(hostPool)
	}
	if s.u8()%4 == 0 {
		r.HostHeader = s.pick(hostPool)
	}
	for n := int(s.u8() % 4); n > 0; n-- {
		r.Headers = append(r.Headers, [2]string{s.pick(hdrPool), s.headerValue(g, value)})
	}
	r.Query = s.compose(g, value, false)
	r.Body = s.compose(g, value, false)
	// One request in four carries a method override header, so check's refusal of one under
	// an entry with an allow list is reached from both sides. The draw is the last one of
	// the case, after the query and the body, so that a seed written before it still names
	// the case it names: a byte string that has run out answers 0 here, and 0 adds nothing.
	if s.u8()%4 == 1 {
		r.Headers = append(r.Headers, [2]string{s.pick(overridePool), s.compose(g, value, false)})
	}
	return wireCase{Rules: rules, Grant: g, Value: value, Request: r}
}

// allValidUTF8: the wire (JSON) and the model (Lean String) carry only valid UTF-8, so a
// case with a byte string that is not is out of scope — Go would send it as U+FFFD and the
// two sides would be compared on different strings. Basic payloads may still decode to
// invalid UTF-8, which is a case both sides do handle, and basicPayload refuses alike.
func allValidUTF8(c wireCase) bool {
	b, _ := json.Marshal(c)
	return utf8.Valid(b) && !bytes.Contains(b, []byte(`\ufffd`))
}

// FuzzAirlock compares the two sides on every case the byte string generates. The committed
// corpus under testdata/fuzz/FuzzAirlock reaches every refusal reason and both answers of
// Validate, so `go test -run FuzzAirlock` is already the differential test; -fuzz goes on
// from those seeds.
func FuzzAirlock(f *testing.F) {
	if modelPath() == "" {
		f.Skip("AIRLOCK_MODEL names no built airlock-model; lake build in lean/ writes one to " + modelBuildPath)
	}
	f.Add([]byte{}) // the empty byte string: no entry, no header, every pool at its head
	f.Fuzz(func(t *testing.T, data []byte) {
		c := genCase(data)
		if !allValidUTF8(c) {
			t.Skip()
		}
		line, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		// Compared as values, not as bytes: Lean's Json.compress prints an object's keys
		// sorted and encoding/json prints a struct's fields in declaration order, so the
		// same answer is two different lines.
		want := leanModel(t).ask(t, line)
		got := goAnswer(c)
		if !reflect.DeepEqual(want, got) {
			wj, _ := json.Marshal(want)
			gj, _ := json.Marshal(got)
			t.Fatalf("Go and the Lean model disagree\ncase: %s\nlean: %s\ngo:   %s", line, wj, gj)
		}
	})
}
