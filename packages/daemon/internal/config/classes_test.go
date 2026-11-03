package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// settingsConfigTypes is Config and every struct type of this package reachable from it through
// exported fields, pointers, slices, arrays and map keys and elements, each once: the types whose
// exported fields TestEverySettingsPathHasAClass holds to the class table. The walk is a copy of
// api/settings_test.go's, which holds the same field set to what the state may carry; the two
// packages cannot share a test helper, and the copy is what makes a field added to Config fail
// both tests rather than one. A type of another package (a Kubernetes API type, time.Duration, a
// role, a backend, a compiled regexp) is a leaf: its fields are no legion.yaml keys.
func settingsConfigTypes() []reflect.Type {
	root := reflect.TypeFor[Config]()
	var types []reflect.Type
	var walk func(typ reflect.Type)
	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Map:
			walk(typ.Key())
			walk(typ.Elem())
		case reflect.Struct:
			if typ.PkgPath() != root.PkgPath() || slices.Contains(types, typ) {
				return
			}
			types = append(types, typ)
			for i := range typ.NumField() {
				if field := typ.Field(i); field.IsExported() {
					walk(field.Type)
				}
			}
		}
	}
	walk(root)
	return types
}

// classesDerived is every exported field the walk reaches that is no legion.yaml key and so has no
// row in Classes: a value the loader computes from a key that has one. Nothing may be added here
// that an operator can write in their file.
var classesDerived = map[string]string{
	"Kubernetes.PluginsFromBinary": "derived: whether runtime.kubernetes.plugins was filled from the binary's linked pair rather than from the file",
	"PathPattern.Regexp":           "derived: compiled from projects.<KEY>.merge.by_hand[], which carries the expression",
}

// Every exported field of every struct the walk reaches is in the class table or in
// classesDerived, and every row of the table names a field the walk reaches. A new field fails
// here until someone has read what the daemon does with its value and said when a change of it
// takes effect.
func TestEverySettingsPathHasAClass(t *testing.T) {
	seen := map[string]bool{}
	for _, typ := range settingsConfigTypes() {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			name := typ.Name() + "." + field.Name
			seen[name] = true
			_, classed := rowOf(name)
			_, derived := classesDerived[name]
			switch {
			case classed && derived:
				t.Errorf("%s is in both Classes and classesDerived", name)
			case !classed && !derived:
				t.Errorf("%s has no class: read what the daemon does with the value, then add a row to Classes in config/classes.go giving its legion.yaml path, its class (decision, rebuild, launch or fixed) and, where something holds it, the holder a change makes the daemon build again; a value the loader derives from another key goes in classesDerived instead", name)
			}
		}
	}
	for _, row := range Classes {
		if !seen[row.Field] {
			t.Errorf("Classes names %s, which is no exported field of a struct reachable from Config: drop the row, or name the field the setting now lands in", row.Field)
		}
	}
	for name := range classesDerived {
		if !seen[name] {
			t.Errorf("classesDerived names %s, which is no exported field of a struct reachable from Config: drop the row", name)
		}
	}
}

// Each row is sound on its own terms: a fixed row says why it is refused and names no holder, a
// rebuild row names the holder it rebuilds, and no other row carries a refusal.
func TestEveryClassRowIsWellFormed(t *testing.T) {
	for _, row := range Classes {
		switch row.Class {
		case ClassFixed:
			if row.Why == "" {
				t.Errorf("%s is fixed with no reason: the route refuses the path by name and prints Why", row.Field)
			}
			if row.Holder != "" {
				t.Errorf("%s is fixed and names the holder %q: nothing is rebuilt for a refused path", row.Field, row.Holder)
			}
		case ClassRebuild:
			if row.Holder == "" {
				t.Errorf("%s is a rebuild with no holder: name what the daemon builds again", row.Field)
			}
		case ClassDecision, ClassLaunch:
		default:
			t.Errorf("%s has the class %q, which is none of the four", row.Field, row.Class)
		}
		if row.Class != ClassFixed && row.Why != "" {
			t.Errorf("%s is %s and carries a refusal reason, which only a fixed row has", row.Field, row.Class)
		}
		if row.Path == "" {
			t.Errorf("%s has no dotted path", row.Field)
		}
	}
}

// Two rows may share a path, where one legion.yaml key fills more than one field; ClassOf answers
// the first, so the rows that share it must agree.
func TestRowsThatShareAPathAgree(t *testing.T) {
	byPath := map[string]Row{}
	for _, row := range Classes {
		first, held := byPath[row.Path]
		if !held {
			byPath[row.Path] = row
			continue
		}
		if first.Class != row.Class || first.Holder != row.Holder || first.Why != row.Why {
			t.Errorf("%s and %s share the path %s and disagree: %+v against %+v", first.Field, row.Field, row.Path, first, row)
		}
	}
}

// Paths is the table's census with no path twice, in order, and it holds the keys an operator
// writes at the top level of the file.
func TestPathsIsTheSortedCensus(t *testing.T) {
	paths := Paths()
	if !slices.IsSorted(paths) {
		t.Errorf("Paths() is not sorted: %q", paths)
	}
	for i := 1; i < len(paths); i++ {
		if paths[i] == paths[i-1] {
			t.Errorf("Paths() holds %s twice", paths[i])
		}
	}
	for _, path := range []string{"admission_cap", "gates.design", "runtime", "runtime.kubernetes.image", "github_apps", "projects"} {
		if !slices.Contains(paths, path) {
			t.Errorf("Paths() does not hold %s", path)
		}
	}
	// Every top-level key the loader reads has a row, so an operator who names one is never told
	// it is unknown.
	for _, key := range loaderTopLevelKeys() {
		if _, known := ClassOf(key); !known {
			t.Errorf("the loader reads %s and the class table has no row for it", key)
		}
	}
}

// loaderTopLevelKeys is every top-level key readKeys reads, read from the loader's own tables and
// from the source of its switch, so a key added there with no class fails this test.
func loaderTopLevelKeys() []string {
	keys := []string{
		"project", "bind", "advertise_host", "postgres_dsn", "state_dir", "port", "admission_cap",
		"runtime", "daemon_url", "omp_invocation", "omp_launch_prefix", "provider_keys",
		"instructions", "worker_stream_port", "operator_token_file", "envoy_url", "nats_urls",
		"envoy_token_file", "nats_nkey_seed_file", "nats_daemon_nkey_seed_file", "dispatch_url",
		"dispatch_token_file", "projects", "gates", "github_apps", "linger_hours",
		"production_check_hours", "pod_update_retask_hours", "pod_update_notice_hours",
		"review_round_cap", "max_fix_attempts", "controller", "capabilities", "worker_backends",
		"role_config_files", repositoryRenamesKey,
	}
	for _, duration := range durationKeys {
		keys = append(keys, duration.key)
	}
	for _, count := range countKeys {
		keys = append(keys, count.key)
	}
	return keys
}

// ClassOf reads a concrete path against the templates the table writes: a name the file chose and
// one entry of a sequence.
func TestClassOfReadsTheTemplatedPaths(t *testing.T) {
	for _, tc := range []struct{ path, field string }{
		{"admission_cap", "Config.AdmissionCap"},
		{"gates.design", "Gates.Design"},
		{"projects.DEMO.merge", "Project.Merge"},
		{"projects.DEMO.merge.by_hand", "MergeRules.ByHand"},
		{"github_apps.implement.app_id", "GitHubApp.AppID"},
		{"runtime.kubernetes.resources.issue", "PodResources.Issue"},
		{"runtime.kubernetes.resources.controller.cpu", "Reservation.CPU"},
		{"runtime.kubernetes.scheduling.tolerations[1].effect", "Toleration.Effect"},
		{"provider_keys.ANTHROPIC_API_KEY", "ProviderKey.Env"},
	} {
		row, known := ClassOf(tc.path)
		if !known {
			t.Errorf("ClassOf(%q) found no row", tc.path)
			continue
		}
		if row.Field != tc.field {
			t.Errorf("ClassOf(%q) = %s, want %s", tc.path, row.Field, tc.field)
		}
	}
	for _, path := range []string{"", "admission", "gates.designn", "projects.DEMO.merge.by_hand[x", "runtime.kubernetes.resources.issue.cpu.core"} {
		if row, known := ClassOf(path); known {
			t.Errorf("ClassOf(%q) = %s, want no row", path, row.Field)
		}
	}
}

// Every path the refusals name is one an operator can write, and each reason says what is in the
// way rather than only that the path is refused.
func TestTheFixedPathsAreTheOnesBootBound(t *testing.T) {
	var fixed []string
	for _, row := range Classes {
		if row.Class == ClassFixed && !slices.Contains(fixed, row.Path) {
			fixed = append(fixed, row.Path)
		}
	}
	slices.Sort(fixed)
	want := []string{
		"bind", "port", "postgres_dsn", "project", "runtime", "runtime.kubernetes",
		"runtime.kubernetes.namespace", "runtime.tmux", "runtime.tmux.take_over", "state_dir",
		"worker_stream_port",
	}
	if !slices.Equal(fixed, want) {
		t.Errorf("the fixed paths are %q, want %q", fixed, want)
	}
	for _, row := range Classes {
		if row.Class == ClassFixed && len(strings.Fields(row.Why)) < 8 {
			t.Errorf("%s is refused with %q, which does not say what is in the way", row.Path, row.Why)
		}
	}
}
