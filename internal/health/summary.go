package health

import "fmt"

// Summary is the single verdict shown above everything else.
//
// The page's first job is answering "do I need to care?" without the reader
// parsing a list. That means one state and one sentence — and it must never
// overstate: a page that shouts when four checks could not run, rather than
// when something is broken, trains people to ignore it.
type Summary struct {
	Level    Level
	Headline string
	Detail   string
}

// Summarise reduces findings to one verdict.
//
// Unknowns never produce an alarming headline. They are reported plainly,
// because "we could not measure this" is a gap in our knowledge, not a fault in
// the user's server.
func Summarise(findings []Finding) Summary {
	var warns, unknowns []Finding
	for _, f := range findings {
		switch f.Level {
		case LevelWarn:
			warns = append(warns, f)
		case LevelUnknown:
			unknowns = append(unknowns, f)
		}
	}

	switch {
	case len(warns) == 1:
		return Summary{LevelWarn, "One thing needs attention", warns[0].Title + "."}

	case len(warns) > 1:
		return Summary{LevelWarn,
			fmt.Sprintf("%d things need attention", len(warns)),
			warns[0].Title + ", and " + plural(len(warns)-1, "other issue", "other issues") + "."}

	case len(unknowns) == 1:
		return Summary{LevelUnknown, "Everything checked looks healthy",
			"One check could not run: " + lower(unknowns[0].Title) + "."}

	case len(unknowns) > 1:
		return Summary{LevelUnknown, "Everything checked looks healthy",
			fmt.Sprintf("%d checks could not run, so parts of the picture are missing.", len(unknowns))}
	}

	return Summary{LevelOK, "Everything looks healthy",
		"All checks passed, including the ones Plex's own integrity check does not cover."}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func lower(s string) string {
	if s == "" {
		return s
	}
	// Only the first letter, so acronyms and product names survive.
	return string(s[0]|0x20) + s[1:]
}
