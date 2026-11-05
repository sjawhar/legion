package capabilities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
)

// overlaysVariable is Oh My Pi's list of settings overlays.
const overlaysVariable = "PI_CONFIG_FILES"

// OverlayEntries are the overlays a PI_CONFIG_FILES value names, in order, its empty entries left
// out as Oh My Pi leaves them out (`a.yml:` and `a.yml::b.yml` start Oh My Pi on the files alone).
func OverlayEntries(value string) []string {
	return slices.DeleteFunc(strings.Split(value, ":"), func(entry string) bool { return entry == "" })
}

// CheckOverlays is whether img's Oh My Pi starts on overlays, a role's whole PI_CONFIG_FILES in
// order, with each of own, the role's own overlays among them, also read alone: refused names the
// first overlay it would not start on and why — a file this process cannot read
// (podsafety.CheckOverlay), or `<Launch> config get retry.modelFallback --json` exiting non-zero
// under PI_CONFIG_FILES set to that file alone, then to the whole list, with Oh My Pi's own error
// line — and is "" when it starts. err joins the runs that never reached an exit of their own (Oh
// My Pi would not start at all, or timed out), which say nothing of the overlays: such a run does
// not stop the check, every later run still goes, and a refusal found after one is still refused;
// a caller reports err rather than refuse on it. img.Env's own PI_CONFIG_FILES is replaced on each
// run.
func CheckOverlays(ctx context.Context, img Image, overlays, own []string) (refused string, err error) {
	for _, file := range overlays {
		if err := podsafety.CheckOverlay(file); err != nil {
			return fmt.Sprintf("%s cannot be read: %v", file, err), nil
		}
	}
	run := func(list []string) (string, error) {
		const args = "config get retry.modelFallback --json"
		r := img.withOverlays(list).launch(ctx, args)
		switch {
		case r.err != nil:
			return "", fmt.Errorf("%s %s under PI_CONFIG_FILES %s: %w", img.Launch, args, strings.Join(list, ":"), r.err)
		case r.exit != 0:
			return errorLine(r), nil
		}
		return "", nil
	}
	var unanswered []error
	for _, file := range own {
		why, err := run([]string{file})
		if why != "" {
			return fmt.Sprintf("Oh My Pi refuses %s: %s", file, why), errors.Join(unanswered...)
		}
		if err != nil {
			unanswered = append(unanswered, err)
		}
	}
	why, err := run(overlays)
	if err != nil {
		unanswered = append(unanswered, err)
	}
	if why != "" {
		why = fmt.Sprintf("Oh My Pi refuses PI_CONFIG_FILES %s: %s", strings.Join(overlays, ":"), why)
	}
	return why, errors.Join(unanswered...)
}

// withOverlays is img with PI_CONFIG_FILES set to list alone.
func (img Image) withOverlays(list []string) Image {
	env := slices.DeleteFunc(slices.Clone(img.Env), func(pair string) bool { return strings.HasPrefix(pair, overlaysVariable+"=") })
	img.Env = append(env, overlaysVariable+"="+strings.Join(list, ":"))
	return img
}

// errorLine is Oh My Pi's own reason for a failed run: the last line of its output beginning
// `error:` (Oh My Pi prints a source excerpt before it, which a cut of the whole output would keep
// in its place), else the run's failure as it printed it.
func errorLine(r ran) string {
	lines := strings.Split(r.stderr+"\n"+r.stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "error:") {
			return oneLine(line)
		}
	}
	return r.failure()
}
