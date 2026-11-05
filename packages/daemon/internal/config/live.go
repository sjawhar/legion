package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// The two sources a settings change comes from.
const (
	// SourceFile is the file changing under the daemon: the file is read again and the stored
	// route overlay is kept, so a route change a file edit does not mention still stands.
	SourceFile = "file"
	// SourceRoute is an operator's or Dispatch's call: the requested paths are merged into the
	// stored overlay, which from then on wins over the file's own value for those paths.
	SourceRoute = "route"
)

// The two values a Change renders in place of one it may not print.
const (
	// withheldValue stands for a credential, the source of one, the path of a file holding one,
	// or an address whose userinfo or query may carry one (Row.Secret).
	withheldValue = "(withheld)"
	// UnsetValue stands for a path the configuration does not carry: a block the file names under
	// one runtime and not the other, or an optional block it drops. A drift reason says the same
	// word of the same situation (runtime/sandbox's launchSettingsDrift), so the settings route's
	// answer and a pod's drift reason never disagree on it.
	UnsetValue = "(unset)"
)

// Change is one setting the daemon applied, as a log line and an answer to the caller read it.
type Change struct {
	// Path is the dotted path as legion.yaml writes it.
	Path string
	// Old and New are the values as a log line shows them, "(withheld)" for a secret and
	// "(unset)" for a path the configuration did not carry.
	Old string
	New string
	// Class is what the daemon can do with the new value, and Outcome what that means for the
	// caller: at-once, rebuilt, or next-launch.
	Class   Class
	Outcome Outcome
	// Holder is what the daemon must build again, "" when nothing holds the value.
	Holder string
	// Source is SourceFile or SourceRoute, and By whoever asked for it ("" for the file).
	Source string
	By     string
}

// Refusal is one requested path the daemon would not change, and why.
type Refusal struct {
	Path   string
	Reason string
}

// NotRecorded is a change the daemon applied and could not store: it is running on the new
// value, and its next restart reads the file's own value for those paths again. The caller is
// answered what happened — the change took effect, and nothing keeps it — rather than a refusal
// of a change the daemon made.
//
// It is declared here, beside Change and Refusal, because it is a third thing an applied change
// can be and both sides of the route read it: internal/settings returns it from the keeper's
// route arm, and internal/api answers for it (api's settingsChange), which otherwise would
// import internal/settings for this error alone.
type NotRecorded struct {
	Paths []string
	Err   error
}

func (e *NotRecorded) Error() string {
	return fmt.Sprintf("the daemon applied the change of %s and could not record it: %v", strings.Join(e.Paths, ", "), e.Err)
}

func (e *NotRecorded) Unwrap() error { return e.Err }

// Live is the daemon's one settings holder: the configuration file, the route overlay laid over
// it, and the resolved Config every reader takes a snapshot of.
//
// Every change goes through Apply, whatever asked for it, and every Apply resolves the whole
// document through the loader, so a route change meets exactly the refusals a file change meets
// and no second resolution path exists. The resolved Config sits behind an atomic pointer: a
// reader never blocks, and a reader that took a snapshot keeps reading one configuration for the
// whole of its decision.
type Live struct {
	path string
	env  func(string) string
	// settled is the resolved configuration, never nil once the holder is built.
	settled atomic.Pointer[Config]
	// normalize is the caller's own fix of what the file says, run over every resolution before
	// it settles (NewLive); nil fixes nothing.
	normalize func(*Config) error
	// mu orders the applies against each other and guards overlay and observers. An observer runs
	// under it, in registration order, so no observer may call Apply, Snapshot excepted.
	mu sync.Mutex
	// overlay is the stored route overlay, dotted path to the value the route asked for. The
	// caller seeds it from its durable store; Apply keeps it current.
	overlay   map[string]any
	observers []func(cfg *Config, applied []Change)
}

// NewLive resolves path once and holds the result.
//
// env is the daemon's environment lookup, as Load takes it. normalize is the caller's own fix of
// what the file says — `legion start`'s absolute state_dir and the plugin pair its binary links
// — which every resolution runs over the loaded configuration before it settles, so a file read
// again under a running daemon settles exactly as this read did; nil fixes nothing. It must
// decide from the configuration alone, since every later resolution runs it again, and a
// resolution whose normalize fails changes nothing.
//
// The holder is built with no route overlay over it: the route changes a restart keeps are laid
// back over the file by the boot's own restore, through the route arm every other change takes
// (internal/settings' Keeper.Restore), so the file is read once here and the daemon resolves it
// a second time only when its store holds a change.
func NewLive(path string, env func(string) string, normalize func(*Config) error) (*Live, error) {
	live := &Live{path: path, env: env, normalize: normalize, overlay: map[string]any{}}
	cfg, err := live.resolve(live.overlay)
	if err != nil {
		return nil, err
	}
	live.settled.Store(&cfg)
	return live, nil
}

// Settled is the holder of one configuration the caller resolved itself, with no file under it:
// the daemon's own tests, which build a Config in memory rather than reading legion.yaml.
// Snapshot answers it, no observer is ever called, and Apply refuses by name — nothing here reads
// a file, so it is no second way to resolve one.
func Settled(cfg Config) *Live {
	live := &Live{overlay: map[string]any{}}
	live.settled.Store(&cfg)
	return live
}

// Snapshot is the configuration the daemon is running on, never nil.
func (l *Live) Snapshot() *Config { return l.settled.Load() }

// WhileSettled runs fn under the holder's lock, with the configuration the daemon is running
// on: work of the caller's own that must not run beside an Apply, because it touches the very
// things an apply's observers rebuild. The daemon's reread of its credential files is that work
// (internal/daemon's rereadCredentials): a file rotated in place names no changed setting, so
// nothing of it reaches an observer, and a reread that ran outside this lock would rebuild a
// holder a route change was rebuilding at the same moment.
//
// fn reads the configuration it is handed and must not call Apply, OnChange or WhileSettled.
func (l *Live) WhileSettled(fn func(cfg *Config)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn(l.settled.Load())
}

// Path is the configuration file this holder reads, "" for a holder built from a configuration
// value (Settled). The file watcher watches the directory holding it (internal/settings).
func (l *Live) Path() string { return l.path }

// FileValues is what the file itself says about each of paths, with no overlay over it: the
// value as written, encoded canonically, and "" for a path the file does not carry. It parses
// the document and reads nothing else — no credential file, no App private key command — so a
// caller may ask it as often as a file changes.
//
// It is how the daemon tells a file that has changed a key from one that has not, which no
// resolved configuration can say once a route change of that key is laid over it: the store of
// route changes keeps the digest of the file's own value at the moment each change was applied,
// and a path whose file value has moved since is the file taking its key back
// (internal/settings).
func (l *Live) FileValues(paths []string) (map[string]string, error) {
	if l.path == "" {
		return nil, errors.New("this settings holder was built from a configuration value (config.Settled) and has no file to read")
	}
	document, err := readDocument(l.path)
	if err != nil {
		return nil, err
	}
	root, err := rootMapping(document)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(paths))
	for _, path := range paths {
		node := root
		for _, name := range strings.Split(path, ".") {
			if node == nil || node.Kind != yaml.MappingNode {
				node = nil
				break
			}
			node = mappingMember(node, name)
		}
		if node == nil || node.Tag == "!!null" {
			values[path] = ""
			continue
		}
		var value any
		if err := node.Decode(&value); err != nil {
			return nil, fmt.Errorf("read the file's own %s: %w", path, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("read the file's own %s: %w", path, err)
		}
		values[path] = string(encoded)
	}
	return values, nil
}

// OnChange registers an observer of every applied change. Apply calls each one after it has
// swapped the pointer, in registration order, so an observer reads the new configuration from its
// argument or from Snapshot and rebuilds what the changes name. An observer runs under the
// holder's lock: it must not call Apply or OnChange.
func (l *Live) OnChange(fn func(cfg *Config, applied []Change)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observers = append(l.observers, fn)
}

// Apply settles one settings change and reports what it did.
//
// source is SourceFile, where the file is read again under the stored route overlay and overlay
// names the paths the file has taken back — each mapped to nil, dropped from the stored overlay
// so the file's own value settles again (internal/settings reads the file's own values and
// decides which those are); a value there is the route's business and is refused. Or source is
// SourceRoute, where overlay is the requested dotted path to value changes: a path the class
// table does not hold is refused as an unknown setting, a fixed path is refused by name with its
// reason, a path mapped to nil is withdrawn from the overlay, and the rest are merged into it.
// by is whoever asked, which every Change carries.
//
// The merged overlay is laid over the file's own document and resolved through the loader. A
// resolution that fails changes nothing: the error comes back with the pointer and the stored
// overlay as they were.
func (l *Live) Apply(source, by string, overlay map[string]any) (applied []Change, refused []Refusal, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	merged := map[string]any{}
	maps.Copy(merged, l.overlay)
	switch source {
	case SourceFile:
		for path, value := range overlay {
			if value != nil {
				return nil, nil, errors.New(`a "file" apply carries no value of its own: it reads the file again, keeps the stored route overlay, and drops only the paths the file has taken back`)
			}
			delete(merged, path)
		}
	case SourceRoute:
		for _, path := range slices.Sorted(maps.Keys(overlay)) {
			switch row, known := ClassOf(path); {
			case !known:
				refused = append(refused, Refusal{Path: path, Reason: "unknown setting"})
			case row.Class == ClassFixed:
				refused = append(refused, Refusal{Path: path, Reason: row.Why})
			case overlay[path] == nil:
				delete(merged, path)
			default:
				merged[path] = overlay[path]
			}
		}
	default:
		return nil, nil, fmt.Errorf("unknown settings source %q: the sources are %q and %q", source, SourceFile, SourceRoute)
	}

	next, err := l.resolve(merged)
	if err != nil {
		return nil, refused, err
	}
	applied = diff(settingsOf(l.settled.Load()), settingsOf(&next), source, by)
	l.overlay = merged
	l.settled.Store(&next)
	for _, observe := range l.observers {
		observe(&next, applied)
	}
	return applied, refused, nil
}

// resolve reads the file again, lays overlay over its document, runs the whole of it through the
// loader, App private keys included, exactly as a boot's Load does, and then the caller's own fix
// of what the file says (Normalize).
func (l *Live) resolve(overlay map[string]any) (Config, error) {
	if l.path == "" {
		return Config{}, errors.New("this settings holder was built from a configuration value (config.Settled) and has no file to read again")
	}
	document, err := readDocument(l.path)
	if err != nil {
		return Config{}, err
	}
	if err := overlayDocument(document, overlay); err != nil {
		return Config{}, err
	}
	cfg, err := loadDocument(document, l.path, l.env, true)
	if err != nil {
		return Config{}, err
	}
	if l.normalize != nil {
		if err := l.normalize(&cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

// overlayDocument lays the overlay over the file's own document, in place: each dotted path names
// a chain of mapping keys, and the last holds the value. A key the file does not carry is created,
// one it carries is replaced, and a null one is filled, so the loader reads the two layers as one
// file. The paths are laid in sorted order, so two paths that meet settle the same way every time.
func overlayDocument(document *yaml.Node, overlay map[string]any) error {
	if len(overlay) == 0 {
		return nil
	}
	if document.Kind == 0 || len(document.Content) == 0 {
		document.Kind = yaml.DocumentNode
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root, err := rootMapping(document)
	if err != nil {
		return err
	}
	for _, path := range slices.Sorted(maps.Keys(overlay)) {
		segments := strings.Split(path, ".")
		mapping := root
		for depth, name := range segments[:len(segments)-1] {
			member := mappingMember(mapping, name)
			if member == nil {
				member = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				mapping.Content = append(mapping.Content, scalarNode(name), member)
			}
			if member.Tag == "!!null" {
				*member = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			if member.Kind != yaml.MappingNode {
				return fmt.Errorf("%s cannot be set: the file's %s is not a mapping", path, strings.Join(segments[:depth+1], "."))
			}
			mapping = member
		}
		var encoded yaml.Node
		if err := encoded.Encode(overlay[path]); err != nil {
			return fmt.Errorf("%s cannot be set: %w", path, err)
		}
		name := segments[len(segments)-1]
		if member := mappingMember(mapping, name); member != nil {
			*member = encoded
			continue
		}
		mapping.Content = append(mapping.Content, scalarNode(name), &encoded)
	}
	return nil
}

// mappingMember is the value node a mapping holds under name, nil when it holds none. A file that
// carries one key twice is read as readKeys reads it — the last member wins — so an overlay laid
// over such a file changes the value the loader would have taken, not the one it discards.
func mappingMember(mapping *yaml.Node, name string) *yaml.Node {
	var member *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			member = mapping.Content[i+1]
		}
	}
	return member
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

// setting is one path's value twice over: as the diff compares it, and as a Change prints it. A
// secret's two differ, so a rotation is seen without being shown.
type setting struct{ compared, shown string }

// settingsOf is cfg by dotted path: one entry per path the class table calls a value rather than a
// block. A block the configuration does not carry contributes nothing, and the diff reads its
// members as unset.
func settingsOf(cfg *Config) map[string]setting {
	settings := map[string]setting{}
	WalkSettings(cfg, func(row Row, value reflect.Value) {
		rendered := Render(value)
		if row.Secret {
			settings[row.Path] = setting{compared: rendered, shown: withheldValue}
			return
		}
		settings[row.Path] = setting{compared: rendered, shown: rendered}
	})
	return settings
}

// WalkSettings visits every value of cfg the class table names, by the row it is read into: one
// visit per path the table calls a value rather than a block, with a block descended into and a
// block the configuration does not carry contributing nothing. A field the table has no row for
// is a derived value that is no legion.yaml key, which config's own census test names.
//
// It is the one walk of the configuration the daemon has: config's own diff reads its settings
// through it (settingsOf), and runtime/sandbox records a pod's launch keys through it
// (sandbox.LaunchSettings), so a row added to the table reaches both and the two can never
// disagree on which fields the table names.
func WalkSettings(cfg *Config, visit func(row Row, value reflect.Value)) {
	walkSettings(reflect.ValueOf(cfg).Elem(), visit)
}

func walkSettings(block reflect.Value, visit func(row Row, value reflect.Value)) {
	typ := block.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		row, known := rowOf(typ.Name() + "." + field.Name)
		if !known {
			continue
		}
		value := block.Field(i)
		if !row.Block {
			visit(row, value)
			continue
		}
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				continue
			}
			value = value.Elem()
		}
		walkSettings(value, visit)
	}
}

// Render is one value as a log line shows it: a duration as its own string, another scalar as it
// is, and anything composite as JSON, whose map keys are sorted and whose pointers are followed.
// fmt's %v would print a pointer's address, which differs between two loads of one file and would
// make every reload report a change.
//
// A pod's recorded launch settings are rendered through it too (sandbox.LaunchSettings): two
// equal configurations must encode identically, since a record that differs is drift.
func Render(value reflect.Value) string {
	if duration, ok := value.Interface().(time.Duration); ok {
		return duration.String()
	}
	switch value.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return fmt.Sprintf("%v", value.Interface())
	}
	encoded, err := json.Marshal(value.Interface())
	if err != nil {
		return fmt.Sprintf("%+v", value.Interface())
	}
	return string(encoded)
}

// Changed is one path two readings of the settings disagree on, with what each reading holds and
// UnsetValue for a reading that does not carry the path at all.
type Changed struct{ Path, Old, New string }

// Compare is every path the two readings of the settings disagree on, in path order. A path one
// reading does not carry reads as UnsetValue there, so a key that arrived or was dropped is a
// change naming what it was and what it is.
//
// It is the one comparison of two readings the daemon has: config's own diff of two resolved
// configurations reads through it, and runtime/sandbox compares a pod's recorded launch keys
// against the runtime's through it (launchSettingsDrift), so the two can never disagree about
// which paths moved or about how a path only one of them carries reads.
func Compare(old, next map[string]string) []Changed {
	paths := slices.Collect(maps.Keys(next))
	for path := range old {
		if _, held := next[path]; !held {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	changed := make([]Changed, 0, len(paths))
	for _, path := range paths {
		before, hadBefore := old[path]
		after, hasAfter := next[path]
		if !hadBefore {
			before = UnsetValue
		}
		if !hasAfter {
			after = UnsetValue
		}
		if before == after {
			continue
		}
		changed = append(changed, Changed{Path: path, Old: before, New: after})
	}
	return changed
}

// diff is one Change per path whose value the two configurations disagree on, in path order. A
// secret is compared by its value and shown as "(withheld)" (setting), so a rotation is a change
// without being printed.
func diff(old, next map[string]setting, source, by string) []Change {
	var changes []Change
	for _, moved := range Compare(comparedValues(old), comparedValues(next)) {
		row, _ := ClassOf(moved.Path)
		changes = append(changes, Change{
			Path: moved.Path, Old: shownValue(old, moved.Path), New: shownValue(next, moved.Path),
			Class: row.Class, Outcome: row.Class.Outcome(), Holder: row.Holder,
			Source: source, By: by,
		})
	}
	return changes
}

// comparedValues is each path's value as the comparison reads it, and shownValue one path's
// value as a Change prints it, UnsetValue for a path the configuration does not carry.
func comparedValues(settings map[string]setting) map[string]string {
	values := make(map[string]string, len(settings))
	for path, value := range settings {
		values[path] = value.compared
	}
	return values
}

func shownValue(settings map[string]setting, path string) string {
	value, held := settings[path]
	if !held {
		return UnsetValue
	}
	return value.shown
}
