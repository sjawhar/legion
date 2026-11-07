package pmdoc

// The display read's half of the pmdoc differential. It is apart from differential_test.go
// because gen/differential.sh copies that one file into the base revision and builds it there,
// so nothing in it may name ReadForDisplay, which a revision before this one does not have. The
// gates this feeds are on the head alone (gen/differential.sh's usage says why).

func init() {
	diffDisplayRead = func(markdown string) (latest, version *Node) {
		return ReadForDisplay(markdown, DisplayLatest), ReadForDisplay(markdown, DisplayVersion)
	}
}
