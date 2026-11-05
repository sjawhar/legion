package sampler

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// contractPath is docs/sampler-metrics.md from this package's directory: the contract a person
// reads, which the tests below read too, so the document and the code cannot drift apart.
var contractPath = filepath.Join("..", "..", "..", "..", "docs", "sampler-metrics.md")

// clock is a clock a test moves by hand, which is how the hour window and the drop order are
// tested without a test that sleeps.
type clock struct{ at time.Time }

func newClock() *clock {
	return &clock{at: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time { return c.at }

func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// series is one line of an exposition read back: the metric, the labels it carries in the order
// they were written, and the number served.
type series struct {
	name   string
	labels []string
	values []string
	value  float64
}

// exposition is one scrape read back, as a test asks questions of it.
type exposition struct {
	text string
	// types and helps are the # TYPE and # HELP lines, by metric name, and names is each
	// metric in the order its lines were written.
	types map[string]string
	helps map[string]string
	names []string
	// series are the lines in the order they were written, and values each line's number by
	// its metric name and its label values.
	series []series
	values map[string]float64
}

// scrape writes the registry's exposition and reads it back. It also refuses an exposition no
// collector could parse, so every test below rests on the format being whole.
func scrape(t *testing.T, r *Registry) exposition {
	t.Helper()
	var out strings.Builder
	if err := r.Write(&out); err != nil {
		t.Fatalf("the exposition could not be written: %v", err)
	}
	read := exposition{
		text:   out.String(),
		types:  map[string]string{},
		helps:  map[string]string{},
		values: map[string]float64{},
	}
	for _, line := range strings.Split(strings.TrimSuffix(read.text, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, found := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !found || help == "" {
				t.Fatalf("a HELP line carries no help: %q", line)
			}
			if _, already := read.helps[name]; already {
				t.Errorf("%s has two HELP lines", name)
			}
			read.helps[name] = help
		case strings.HasPrefix(line, "# TYPE "):
			name, typ, found := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !found {
				t.Fatalf("a TYPE line carries no type: %q", line)
			}
			if _, already := read.types[name]; already {
				t.Errorf("%s has two TYPE lines", name)
			}
			read.types[name] = typ
			read.names = append(read.names, name)
		default:
			one := parseSeries(t, line)
			if _, already := read.types[one.name]; !already {
				t.Errorf("%s is served before its HELP and TYPE lines", one.name)
			}
			read.series = append(read.series, one)
			read.values[key(one.name, one.values...)] = one.value
		}
	}
	return read
}

// parseSeries reads one series line, `name{label="value",…} number`, with the escapes of the
// text format read back, so a test sees the label value the sampler was handed.
func parseSeries(t *testing.T, line string) series {
	t.Helper()
	text, number, found := strings.Cut(line, " ")
	if !found {
		t.Fatalf("a series line carries no value: %q", line)
	}
	value, err := strconv.ParseFloat(number, 64)
	if err != nil {
		t.Fatalf("the value of %q is no number: %v", line, err)
	}
	name, labels, cut := strings.Cut(text, "{")
	one := series{name: name, value: value}
	if !cut {
		return one
	}
	if !strings.HasSuffix(labels, "}") {
		t.Fatalf("the labels of %q are not closed", line)
	}
	labels = strings.TrimSuffix(labels, "}")
	for at := 0; at < len(labels); {
		equals := strings.IndexByte(labels[at:], '=')
		if equals < 0 || at+equals+1 >= len(labels) || labels[at+equals+1] != '"' {
			t.Fatalf("the labels of %q are not name=\"value\" pairs", line)
		}
		one.labels = append(one.labels, labels[at:at+equals])
		at += equals + 2
		var value strings.Builder
		for at < len(labels) && labels[at] != '"' {
			if labels[at] == '\\' {
				at++
				if at == len(labels) {
					t.Fatalf("a label value of %q ends in a backslash", line)
				}
				if labels[at] == 'n' {
					value.WriteByte('\n')
					at++
					continue
				}
			}
			value.WriteByte(labels[at])
			at++
		}
		if at == len(labels) {
			t.Fatalf("a label value of %q is not closed", line)
		}
		one.values = append(one.values, value.String())
		at++
		if at < len(labels) && labels[at] == ',' {
			at++
		}
	}
	return one
}

// key is one series as the tests name it: the metric and its label values, in the metric's own
// order.
func key(name string, values ...string) string {
	return name + "{" + strings.Join(values, "|") + "}"
}

// value is the number the exposition serves for one series, and a failure where it serves none.
func (e exposition) value(t *testing.T, id metricID, values ...string) float64 {
	t.Helper()
	value, served := e.values[key(metrics[id].name, values...)]
	if !served {
		t.Fatalf("%s is not served with the labels %v:\n%s", metrics[id].name, values, e.text)
	}
	return value
}

// has reports whether the exposition serves one series.
func (e exposition) has(id metricID, values ...string) bool {
	_, served := e.values[key(metrics[id].name, values...)]
	return served
}

// documented is one row of a table of docs/sampler-metrics.md.
type documented struct {
	typ    string
	labels []string
	modes  Mode
}

// contractTables are the sections of docs/sampler-metrics.md that hold a metric table, and the
// modes each table's metrics are served in. A table under any other heading is a failure: a
// metric a reader finds in the document has to be one the sampler serves.
var contractTables = map[string]Mode{
	"The series an issue pod serves":             ModePod,
	"The series node mode serves":                ModeNode,
	"The series the sampler serves about itself": bothModes,
}

// readContract is every metric docs/sampler-metrics.md names, read from its tables.
func readContract(t *testing.T) map[string]documented {
	t.Helper()
	text, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("the contract document could not be read: %v", err)
	}
	rows := map[string]documented{}
	section := ""
	for _, line := range strings.Split(string(text), "\n") {
		if heading, found := strings.CutPrefix(line, "## "); found {
			section = strings.TrimSpace(heading)
			continue
		}
		if heading, found := strings.CutPrefix(line, "### "); found {
			section = strings.TrimSpace(heading)
			continue
		}
		if !strings.HasPrefix(line, "| `legion_sampler_") {
			continue
		}
		modes, known := contractTables[section]
		if !known {
			t.Fatalf("the section %q holds a metric row, and no test knows which mode it is served in:\n%s", section, line)
		}
		cells := tableCells(line)
		if len(cells) != 4 {
			t.Fatalf("a metric row holds %d cells, not four: %s", len(cells), line)
		}
		name := bare(cells[0])
		if _, twice := rows[name]; twice {
			t.Errorf("%s has two rows in the document", name)
		}
		rows[name] = documented{typ: cells[1], labels: documentedLabels(cells[2]), modes: modes}
	}
	if len(rows) == 0 {
		t.Fatal("the contract document names no metric at all")
	}
	return rows
}

// tableCells are the cells of one markdown table row, trimmed.
func tableCells(line string) []string {
	row := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")
	cells := strings.Split(row, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// bare is a cell's text without the backticks a document writes a name in.
func bare(cell string) string { return strings.Trim(cell, "`") }

// documentedLabels are the labels one row names, and none where it holds an em dash.
func documentedLabels(cell string) []string {
	if cell == "—" {
		return nil
	}
	var labels []string
	for _, label := range strings.Split(cell, ",") {
		labels = append(labels, bare(strings.TrimSpace(label)))
	}
	return labels
}

// The document and the code carry one contract. This reads docs/sampler-metrics.md's tables and
// compares them with the metric table in series.go: the name, the type, the labels and the mode
// of every metric, both ways, so neither a metric added to the code alone nor one left in the
// document alone passes.
func TestTheDocumentedContractIsTheDeclaredContract(t *testing.T) {
	rows := readContract(t)
	for id := range metricCount {
		m := &metrics[id]
		row, found := rows[m.name]
		if !found {
			t.Errorf("%s is declared in series.go and named in no table of %s", m.name, contractPath)
			continue
		}
		if row.typ != m.typ {
			t.Errorf("%s is a %s in the document and a %s in series.go", m.name, row.typ, m.typ)
		}
		if !slices.Equal(row.labels, m.labels) {
			t.Errorf("%s carries %v in the document and %v in series.go", m.name, row.labels, m.labels)
		}
		if row.modes != m.modes {
			t.Errorf("%s is served in mode %d by the document and in mode %d by series.go", m.name, row.modes, m.modes)
		}
	}
	declared := map[string]bool{}
	for id := range metricCount {
		declared[metrics[id].name] = true
	}
	for name := range rows {
		if !declared[name] {
			t.Errorf("%s is named in %s and declared nowhere in series.go", name, contractPath)
		}
	}
}

// Every name the document writes in its prose is a metric too: a sentence about a series the
// sampler does not serve is as wrong as a missing row, and costs whoever reads the contract the
// same hour.
func TestTheDocumentNamesNoMetricThatDoesNotExist(t *testing.T) {
	text, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("the contract document could not be read: %v", err)
	}
	declared := map[string]bool{}
	for id := range metricCount {
		declared[metrics[id].name] = true
	}
	for _, name := range regexp.MustCompile(`legion_sampler_[a-z_]+`).FindAllString(string(text), -1) {
		if !declared[name] {
			t.Errorf("%s names %s, which the sampler does not serve", contractPath, name)
		}
	}
}

// What a registry serves is what the document's tables say it serves, for each mode: the set of
// metrics, their types and the labels each series carries. The registry is given one reading of
// every metric, and also one of the other mode's, which must reach no collector.
func TestTheServedSetIsTheDocumentedSet(t *testing.T) {
	rows := readContract(t)
	for _, test := range []struct {
		name     string
		mode     Mode
		registry func(now func() time.Time) *Registry
	}{
		{"an issue pod", ModePod, func(now func() time.Time) *Registry {
			return NewPodRegistry("INFRA", "INFRA-1208", now)
		}},
		{"node mode", ModeNode, func(now func() time.Time) *Registry {
			return NewNodeRegistry(now)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			at := newClock()
			registry := test.registry(at.now)
			fillPod(registry)
			fillNode(registry)
			fillSelf(registry)

			read := scrape(t, registry)
			want := []string{}
			for name, row := range rows {
				if row.modes&test.mode != 0 {
					want = append(want, name)
				}
			}
			slices.Sort(want)
			served := append([]string{}, read.names...)
			slices.Sort(served)
			if !slices.Equal(served, want) {
				t.Errorf("the exposition serves\n%v\nand the document names\n%v", served, want)
			}
			for name, typ := range read.types {
				if row := rows[name]; row.typ != typ {
					t.Errorf("%s is served as a %s and documented as a %s", name, typ, row.typ)
				}
			}
			for _, one := range read.series {
				if labels := rows[one.name].labels; !slices.Equal(one.labels, labels) {
					t.Errorf("%s is served with the labels %v and documented with %v", one.name, one.labels, labels)
				}
			}
		})
	}
}

// fillPod gives a registry one reading of every series an issue pod serves.
func fillPod(r *Registry) {
	pair := Pair{Role: "implement", Kind: KindBuild}
	r.AddCPUSeconds(pair, 3)
	r.SetAnonymousBytes(pair, 2<<20)
	r.SetResidentBytes(pair, 4<<20)
	r.SetVolumeBytes("issue", "disk", RoleShared, 8<<20)
	r.SetPodMemoryBytes(900 << 20)
	r.SetPodCPUSeconds(238)
	r.SetPodProcessMemoryBytes(700 << 20)
	r.SetPodSharedMemoryBytes(57344)
}

// fillNode gives a registry one reading of every series node mode serves.
func fillNode(r *Registry) {
	pod := GradePod{Pod: "grade-7", Namespace: "grade", Queue: "grade-queue", Owner: "lean"}
	r.AddGradeCPUSeconds(pod, KindBuild, 12)
	r.SetGradeResidentBytes(pod, KindBuild, 64<<20)
	r.SetGradePodMemoryBytes(pod, 128<<20)
	r.SetGradePodCPUSeconds(pod, 41)
}

// fillSelf gives a registry one reading of every series the sampler serves about itself.
func fillSelf(r *Registry) {
	r.SetSelfCPUSeconds(0.5)
	r.SetSelfMemoryBytes(30<<20, 102<<20)
	r.AddReadError(SourceProc)
	r.SetRestarts(2)
	r.SetSmapsDeferred(7)
}

// A peak rises with the value it stands beside and is the most held since the last scrape: a
// scrape takes it back to the value, so a second scrape straight after reads the value and not
// the peak the first one reported.
func TestAPeakRisesWithTheValueAndIsResetByAScrape(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	pair := Pair{Role: "implement", Kind: KindTest}
	labels := []string{"INFRA", "INFRA-1208", "implement", string(KindTest)}

	registry.SetResidentBytes(pair, 100)
	registry.SetResidentBytes(pair, 300)
	registry.SetResidentBytes(pair, 200)
	registry.SetAnonymousBytes(pair, 50)
	registry.SetAnonymousBytes(pair, 40)
	registry.SetPodMemoryBytes(900)
	registry.SetPodMemoryBytes(500)
	registry.SetPodProcessMemoryBytes(800)
	registry.SetPodProcessMemoryBytes(400)

	first := scrape(t, registry)
	if got := first.value(t, memoryResidentBytes, labels...); got != 200 {
		t.Errorf("the resident reading is %v, not the last one set, 200", got)
	}
	if got := first.value(t, memoryResidentPeakBytes, labels...); got != 300 {
		t.Errorf("the resident peak is %v, not the most held, 300", got)
	}
	if got := first.value(t, memoryAnonymousPeakBytes, labels...); got != 50 {
		t.Errorf("the anonymous peak is %v, not the most held, 50", got)
	}
	if got := first.value(t, podMemoryPeakBytes, "INFRA", "INFRA-1208"); got != 900 {
		t.Errorf("the pod's memory peak is %v, not the most held, 900", got)
	}
	if got := first.value(t, podProcessMemoryPeakBytes, "INFRA", "INFRA-1208"); got != 800 {
		t.Errorf("the pod's process memory peak is %v, not the most held, 800", got)
	}

	second := scrape(t, registry)
	for _, test := range []struct {
		id     metricID
		labels []string
		want   float64
	}{
		{memoryResidentPeakBytes, labels, 200},
		{memoryAnonymousPeakBytes, labels, 40},
		{podMemoryPeakBytes, []string{"INFRA", "INFRA-1208"}, 500},
		{podProcessMemoryPeakBytes, []string{"INFRA", "INFRA-1208"}, 400},
	} {
		if got := second.value(t, test.id, test.labels...); got != test.want {
			t.Errorf("after a scrape %s is %v, not the reading beside it, %v", metrics[test.id].name, got, test.want)
		}
	}

	registry.SetResidentBytes(pair, 250)
	third := scrape(t, registry)
	if got := third.value(t, memoryResidentPeakBytes, labels...); got != 250 {
		t.Errorf("the peak of the next window is %v, not the most held in it, 250", got)
	}
}

// A grade pod's peaks reset on the same rule, which is what node mode's dashboards read.
func TestAGradePodsPeakIsResetByAScrape(t *testing.T) {
	at := newClock()
	registry := NewNodeRegistry(at.now)
	pod := GradePod{Pod: "grade-7", Namespace: "grade", Queue: "grade-queue", Owner: "lean"}
	labels := []string{"grade-7", "grade", string(KindBuild), "grade-queue", "lean"}

	registry.SetGradeResidentBytes(pod, KindBuild, 900)
	registry.SetGradeResidentBytes(pod, KindBuild, 300)
	registry.SetGradePodMemoryBytes(pod, 1000)
	registry.SetGradePodMemoryBytes(pod, 400)

	first := scrape(t, registry)
	if got := first.value(t, gradeMemoryResidentPeakBytes, labels...); got != 900 {
		t.Errorf("the kind's resident peak is %v, not the most held, 900", got)
	}
	if got := first.value(t, gradePodMemoryPeakBytes, "grade-7", "grade", "grade-queue", "lean"); got != 1000 {
		t.Errorf("the pod's memory peak is %v, not the most held, 1000", got)
	}

	second := scrape(t, registry)
	if got := second.value(t, gradeMemoryResidentPeakBytes, labels...); got != 300 {
		t.Errorf("after a scrape the kind's peak is %v, not the reading beside it, 300", got)
	}
	if got := second.value(t, gradePodMemoryPeakBytes, "grade-7", "grade", "grade-queue", "lean"); got != 400 {
		t.Errorf("after a scrape the pod's peak is %v, not the reading beside it, 400", got)
	}
}

// A pair is served while a process of it ran within the last hour and leaves the output after
// that. The boundary is the hour itself: a pair last seen exactly an hour ago is still served.
func TestAPairLeavesTheOutputAnHourAfterItsLastProcess(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	old := Pair{Role: "implement", Kind: KindBuild}
	recent := Pair{Role: "implement", Kind: KindAgent}

	registry.AddCPUSeconds(old, 30)
	registry.SetResidentBytes(old, 1<<20)
	at.advance(30 * time.Minute)
	registry.AddCPUSeconds(recent, 10)

	at.advance(30 * time.Minute)
	read := scrape(t, registry)
	if !read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", string(KindBuild)) {
		t.Error("a pair whose process ran exactly an hour ago is not served, and the window is the hour itself")
	}

	at.advance(time.Nanosecond)
	read = scrape(t, registry)
	for _, id := range []metricID{cpuSecondsTotal, memoryResidentBytes, memoryResidentPeakBytes} {
		if read.has(id, "INFRA", "INFRA-1208", "implement", string(KindBuild)) {
			t.Errorf("%s is still served for a pair no process of which has run for an hour", metrics[id].name)
		}
	}
	if !read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", string(KindAgent)) {
		t.Error("a pair whose process ran half an hour ago is not served")
	}

	at.advance(31 * time.Minute)
	registry.AddCPUSeconds(old, 5)
	read = scrape(t, registry)
	if got := read.value(t, cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", string(KindBuild)); got != 5 {
		t.Errorf("a pair that returned after the window is served %v seconds, not the 5 it has spent since", got)
	}
	if read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", string(KindAgent)) {
		t.Error("the other pair is still served an hour after its last process")
	}
}

// A grade pod's kinds keep the same window.
func TestAGradePodsKindLeavesTheOutputAfterAnHour(t *testing.T) {
	at := newClock()
	registry := NewNodeRegistry(at.now)
	pod := GradePod{Pod: "grade-7", Namespace: "grade", Queue: "grade-queue", Owner: "lean"}
	registry.AddGradeCPUSeconds(pod, KindBuild, 12)
	registry.SetGradePodMemoryBytes(pod, 1<<20)

	at.advance(time.Hour + time.Second)
	read := scrape(t, registry)
	if read.has(gradeCPUSecondsTotal, "grade-7", "grade", string(KindBuild), "grade-queue", "lean") {
		t.Error("a kind of a grade pod is still served an hour after its last process")
	}
	if !read.has(gradePodMemoryBytes, "grade-7", "grade", "grade-queue", "lean") {
		t.Error("the pod's own totals left the output with its kinds, and only the API server takes a pod away")
	}

	registry.ForgetGradePod(pod)
	read = scrape(t, registry)
	if read.has(gradePodMemoryBytes, "grade-7", "grade", "grade-queue", "lean") {
		t.Error("a pod that is gone is still served")
	}
}

// The cap drops role and kind pairs with the oldest activity first, counts every series it
// removed as legion_sampler_series_dropped, and leaves the pod's totals and the sampler's own
// series alone.
func TestTheCapDropsTheOldestPairsFirstAndCountsThem(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	fillSelf(registry)
	registry.SetPodMemoryBytes(900 << 20)
	registry.SetPodCPUSeconds(238)
	registry.SetPodProcessMemoryBytes(700 << 20)
	registry.SetPodSharedMemoryBytes(57344)
	registry.SetVolumeBytes("issue", "disk", RoleShared, 8<<20)

	// Forty pairs of five series each, one minute apart, is 200 droppable series beside the
	// sixteen the cap may never drop and the one volume.
	for i := range 40 {
		pair := Pair{Role: "implement", Kind: Kind("k" + strconv.Itoa(100+i))}
		registry.AddCPUSeconds(pair, 1)
		registry.SetAnonymousBytes(pair, 1<<20)
		registry.SetResidentBytes(pair, 2<<20)
		at.advance(time.Minute)
	}

	read := scrape(t, registry)
	// 217 series stand against a cap of 200, so the four oldest pairs go and the fifth
	// stays: dropping a fifth pair would be 192, further under the cap than it has to be.
	if got := read.value(t, seriesDropped); got != 20 {
		t.Errorf("the cap removed %v series, not the twenty of the four oldest pairs", got)
	}
	if served := len(read.series); served != 197 {
		t.Errorf("%d series are served, not the 197 the cap leaves", served)
	}
	for i := range 4 {
		kind := "k" + strconv.Itoa(100+i)
		if read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", kind) {
			t.Errorf("the pair %s is served, and it is among the four whose processes ran longest ago", kind)
		}
	}
	for i := 4; i < 40; i++ {
		kind := "k" + strconv.Itoa(100+i)
		if !read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", kind) {
			t.Errorf("the pair %s is not served, and it is newer than the four the cap dropped", kind)
		}
	}
	if !read.has(volumeUsedBytes, "INFRA", "INFRA-1208", "issue", "disk", RoleShared) {
		t.Error("a volume was dropped while pairs were still there, and volumes go last")
	}
	for _, id := range []metricID{podMemoryBytes, podCPUSecondsTotal, podProcessMemoryBytes, podSharedMemoryBytes} {
		if !read.has(id, "INFRA", "INFRA-1208") {
			t.Errorf("%s was dropped, and the pod's totals never are", metrics[id].name)
		}
	}
	for _, id := range []metricID{selfCPUSecondsTotal, selfMemoryAnonymousBytes, selfMemoryResidentBytes, restartsTotal, smapsDeferred} {
		if !read.has(id) {
			t.Errorf("%s was dropped, and the sampler's own series never are", metrics[id].name)
		}
	}
}

// Volumes are dropped after every pair, by name, and a cap that drops everything it may still
// serves the pod's totals and the sampler's own series.
func TestTheCapDropsVolumesLastAndNeverTheTotals(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	fillSelf(registry)
	registry.SetPodMemoryBytes(900 << 20)
	registry.SetPodCPUSeconds(238)
	registry.SetPodProcessMemoryBytes(700 << 20)
	registry.SetPodSharedMemoryBytes(57344)

	pair := Pair{Role: "implement", Kind: KindBuild}
	registry.AddCPUSeconds(pair, 1)
	registry.SetResidentBytes(pair, 2<<20)
	at.advance(time.Minute)
	for i := range 190 {
		registry.SetVolumeBytes("volume-"+strconv.Itoa(100+i), "memory", RoleShared, 1<<20)
	}

	// Sixteen series the cap may never drop, three of the one pair and 190 volumes is 209.
	// The pair goes first, whole, and then the six volumes whose names come first.
	read := scrape(t, registry)
	if got := read.value(t, seriesDropped); got != 9 {
		t.Errorf("the cap removed %v series, not the three of the pair and the six oldest volumes", got)
	}
	if read.has(cpuSecondsTotal, "INFRA", "INFRA-1208", "implement", string(KindBuild)) {
		t.Error("the pair is served while volumes were dropped, and every pair goes before the first volume")
	}
	for i := range 6 {
		name := "volume-" + strconv.Itoa(100+i)
		if read.has(volumeUsedBytes, "INFRA", "INFRA-1208", name, "memory", RoleShared) {
			t.Errorf("%s is served, and volumes are dropped by name", name)
		}
	}
	for i := 6; i < 190; i++ {
		name := "volume-" + strconv.Itoa(100+i)
		if !read.has(volumeUsedBytes, "INFRA", "INFRA-1208", name, "memory", RoleShared) {
			t.Errorf("%s is not served, and the cap had already come down to 200", name)
		}
	}
	if served := len(read.series); served != 200 {
		t.Errorf("%d series are served, not the 200 of the cap", served)
	}
	for _, id := range []metricID{podMemoryBytes, podMemoryPeakBytes, podCPUSecondsTotal, podProcessMemoryBytes, podProcessMemoryPeakBytes, podSharedMemoryBytes} {
		if !read.has(id, "INFRA", "INFRA-1208") {
			t.Errorf("%s was dropped, and the pod's totals never are", metrics[id].name)
		}
	}
	for _, id := range []metricID{selfCPUSecondsTotal, selfMemoryAnonymousBytes, selfMemoryResidentBytes, restartsTotal, seriesDropped, smapsDeferred} {
		if !read.has(id) {
			t.Errorf("%s was dropped, and the sampler's own series never are", metrics[id].name)
		}
	}
}

// The cap is for one pod, and in node mode each grade pod has its own: a node serving many pods
// is not capped as though its pods were one.
func TestTheCapIsPerGradePod(t *testing.T) {
	at := newClock()
	registry := NewNodeRegistry(at.now)
	fillSelf(registry)
	for i := range 40 {
		pod := GradePod{Pod: "grade-" + strconv.Itoa(100+i), Namespace: "grade", Queue: "q", Owner: "lean"}
		registry.SetGradePodMemoryBytes(pod, 1<<20)
		registry.SetGradePodCPUSeconds(pod, 1)
		for _, kind := range []Kind{KindBuild, KindTest, KindOther} {
			registry.AddGradeCPUSeconds(pod, kind, 1)
			registry.SetGradeResidentBytes(pod, kind, 1<<20)
		}
	}

	read := scrape(t, registry)
	if got := read.value(t, seriesDropped); got != 0 {
		t.Errorf("the cap removed %v series from a node whose pods are each far under it", got)
	}
	// Forty pods of twelve series, and the sampler's own ten.
	if served := len(read.series); served != 490 {
		t.Errorf("%d series are served, not the 490 of forty grade pods and the sampler itself", served)
	}
}

// Two scrapes of one unchanged state are the same bytes, every metric carries one HELP and one
// TYPE line before its series, and the series of a metric are sorted.
func TestTheExpositionIsSortedAndStable(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	fillSelf(registry)
	fillPod(registry)
	for _, name := range []string{"work", "cache", "issue", "home"} {
		registry.SetVolumeBytes(name, "memory", RoleShared, 1<<20)
	}
	for _, role := range []string{"review", "implement", "plan"} {
		for _, kind := range []Kind{KindTool, KindAgent, KindBuild} {
			registry.AddCPUSeconds(Pair{Role: role, Kind: kind}, 1)
			registry.SetResidentBytes(Pair{Role: role, Kind: kind}, 1<<20)
		}
	}

	// The first scrape takes every peak down to the reading beside it, so the two after it
	// are scrapes of one unchanged state.
	scrape(t, registry)
	first := scrape(t, registry)
	second := scrape(t, registry)
	if first.text != second.text {
		t.Errorf("two scrapes of one state differ:\n%s\n%s", first.text, second.text)
	}

	for name := range first.types {
		if _, has := first.helps[name]; !has {
			t.Errorf("%s has a TYPE line and no HELP line", name)
		}
	}
	if len(first.names) != len(first.types) {
		t.Errorf("%d TYPE lines carry %d metrics between them", len(first.names), len(first.types))
	}

	seen := map[string][]string{}
	for _, one := range first.series {
		if last, served := seen[one.name]; served && slices.Compare(last, one.values) >= 0 {
			t.Errorf("%s serves %v after %v, and its series are sorted by their labels", one.name, one.values, last)
		}
		seen[one.name] = one.values
	}

	// A metric's lines stand together, in the order the contract's table declares them.
	order := map[string]int{}
	for id := range metricCount {
		order[metrics[id].name] = int(id)
	}
	for i := range len(first.names) - 1 {
		if order[first.names[i]] >= order[first.names[i+1]] {
			t.Errorf("%s is served before %s, and the metrics keep the order of their table",
				first.names[i+1], first.names[i])
		}
	}
}

// A label value holding a quote, a backslash and a newline is escaped as the text format asks,
// and reads back as the value the sampler was handed.
func TestALabelValueIsEscaped(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	name := "a\"b\\c\nd"
	registry.SetVolumeBytes(name, "memory", RoleShared, 42)

	read := scrape(t, registry)
	want := `legion_sampler_volume_used_bytes{project="INFRA",issue="INFRA-1208",volume="a\"b\\c\nd",medium="memory",role="shared"} 42`
	if !strings.Contains(read.text, want+"\n") {
		t.Errorf("the escaped series is not in the exposition:\nwant %s\ngot\n%s", want, read.text)
	}
	if got := read.value(t, volumeUsedBytes, "INFRA", "INFRA-1208", name, "memory", RoleShared); got != 42 {
		t.Errorf("the escaped series reads back as %v, not the 42 it was given", got)
	}
	if strings.Count(read.text, "\n") != len(strings.Split(strings.TrimSuffix(read.text, "\n"), "\n")) {
		t.Error("a newline in a label value ended a line of the exposition")
	}
}

// A counter is never served lower than it was served before, whichever way it is written: a rise
// handed to it, or a whole count read from a file that a remade cgroup or a restarted process
// takes back to zero.
func TestACounterIsNeverServedLowerThanBefore(t *testing.T) {
	at := newClock()
	registry := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	pair := Pair{Role: "implement", Kind: KindBuild}
	labels := []string{"INFRA", "INFRA-1208", "implement", string(KindBuild)}

	registry.AddCPUSeconds(pair, 10)
	registry.SetPodCPUSeconds(100)
	registry.SetSelfCPUSeconds(5)
	registry.SetRestarts(3)
	registry.SetReadErrors(SourceProc, 7)
	first := scrape(t, registry)

	registry.AddCPUSeconds(pair, -4)
	registry.SetPodCPUSeconds(2)
	registry.SetSelfCPUSeconds(1)
	registry.SetRestarts(0)
	registry.SetReadErrors(SourceProc, 0)
	second := scrape(t, registry)

	for _, test := range []struct {
		id     metricID
		labels []string
		want   float64
	}{
		{cpuSecondsTotal, labels, 10},
		{podCPUSecondsTotal, []string{"INFRA", "INFRA-1208"}, 100},
		{selfCPUSecondsTotal, nil, 5},
		{restartsTotal, nil, 3},
		{readErrorsTotal, []string{string(SourceProc)}, 7},
	} {
		if got := first.value(t, test.id, test.labels...); got != test.want {
			t.Errorf("%s is first served as %v, not the %v it was given", metrics[test.id].name, got, test.want)
		}
		if got := second.value(t, test.id, test.labels...); got != test.want {
			t.Errorf("%s fell to %v after a lower reading, and a counter never goes backwards", metrics[test.id].name, got)
		}
	}

	registry.AddCPUSeconds(pair, 2)
	registry.SetPodCPUSeconds(120)
	third := scrape(t, registry)
	if got := third.value(t, cpuSecondsTotal, labels...); got != 12 {
		t.Errorf("the pair's counter is %v, not the 12 of a rise charged on top of its 10", got)
	}
	if got := third.value(t, podCPUSecondsTotal, "INFRA", "INFRA-1208"); got != 120 {
		t.Errorf("the pod's counter is %v, not the 120 the cgroup now reports", got)
	}
}

// A reading handed to the other mode's method reaches no collector: node mode serves no volume
// and an issue pod serves no grade pod, whatever a caller does.
func TestAReadingOfTheOtherModeIsNotServed(t *testing.T) {
	at := newClock()
	node := NewNodeRegistry(at.now)
	fillPod(node)
	read := scrape(t, node)
	for _, id := range []metricID{cpuSecondsTotal, volumeUsedBytes, podMemoryBytes} {
		if _, served := read.types[metrics[id].name]; served {
			t.Errorf("node mode serves %s, which is an issue pod's", metrics[id].name)
		}
	}

	pod := NewPodRegistry("INFRA", "INFRA-1208", at.now)
	fillNode(pod)
	read = scrape(t, pod)
	for _, id := range []metricID{gradeCPUSecondsTotal, gradePodMemoryBytes} {
		if _, served := read.types[metrics[id].name]; served {
			t.Errorf("an issue pod serves %s, which is node mode's", metrics[id].name)
		}
	}
}

// Every scrape carries the content type a collector reads the text format as.
func TestTheContentTypeIsTheTextFormats(t *testing.T) {
	if ContentType != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("the content type is %q, and a collector reads text/plain; version=0.0.4; charset=utf-8", ContentType)
	}
}
