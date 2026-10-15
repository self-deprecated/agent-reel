package reel

var niriHighlightHelp = cliCommand{Name: "service niri-highlight",
	Summary: `Integration: highlight Niri windows of recorded Zellij sessions by writing a
generated include (highlight.include) from the feed socket (default
feed.runtime/feed.sock). Terminal titles are matched as NAME + separator.`,
	Examples: []string{"areel service niri-highlight", "areel service niri-highlight --reload"},
	Docs:     "NIRI.md"}
