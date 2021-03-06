package cmd

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sahib/brig/cmd/tabwriter"

	"github.com/fatih/color"
	"github.com/sahib/brig/client"
	"github.com/urfave/cli"
)

func handleReset(ctx *cli.Context, ctl *client.Client) error {
	force := ctx.Bool("force")
	path := ctx.Args().First()
	rev := "HEAD"

	if len(ctx.Args()) > 1 {
		rev = ctx.Args().Get(1)
	}

	if err := ctl.Reset(path, rev, force); err != nil {
		return ExitCode{UnknownError, fmt.Sprintf("unpin: %v", err)}
	}

	return nil
}

func commitName(cmt *client.Commit) string {
	if len(cmt.Tags) > 0 {
		return strings.ToUpper(cmt.Tags[0])
	}

	return cmt.Hash.ShortB58()
}

func handleHistory(ctx *cli.Context, ctl *client.Client) error {
	path := ctx.Args().First()

	history, err := ctl.History(path)
	if err != nil {
		return ExitCode{UnknownError, fmt.Sprintf("history: %v", err)}
	}

	if _, err := ctl.Stat(path); err != nil {
		fmt.Printf("%s %s",
			color.YellowString("WARNING:"),
			`This file is not part of this commit, but there's still history for it.
         Most likely this file was moved or removed in the past.

`)
	}

	tabW := tabwriter.NewWriter(
		os.Stdout, 0, 0, 2, ' ',
		tabwriter.StripEscape,
	)

	containsMoves := false
	for _, entry := range history {
		for _, detail := range entry.Mask {
			if detail == "moved" {
				containsMoves = true
				break
			}
		}

		if containsMoves {
			break
		}
	}

	if len(history) != 0 {
		if containsMoves {
			fmt.Fprintf(tabW, "CHANGE\tFROM\tTO\tHOW\tWHEN\t\n")
		} else {
			fmt.Fprintf(tabW, "CHANGE\tFROM\tTO\t\tWHEN\t\n")
		}
	}

	for idx, entry := range history {
		what := ""
		printLine := true

		for _, detail := range entry.Mask {
			// If it was moved, let's display what moved.
			if detail == "moved" && idx+1 < len(history) {
				src := history[idx+1].Path
				dst := entry.Path

				if entry.ReferTo != "" {
					dst = entry.ReferTo
				}

				what = fmt.Sprintf(
					"%s → %s", color.RedString(src), color.RedString(dst),
				)
			}

			// Only display empty changes if nothing happened.
			if detail == "none" && !ctx.Bool("empty") {
				printLine = false
			}
		}
		if !printLine {
			continue
		}

		changeDesc := color.YellowString(strings.Join(entry.Mask, ", "))
		when := color.MagentaString(entry.Head.Date.Format(time.Stamp))

		fmt.Fprintf(
			tabW,
			"%s\t%s\t%s\t%s\t%s\t\n",
			changeDesc,
			color.CyanString(commitName(entry.Next)),
			color.GreenString(commitName(entry.Head)),
			what,
			when,
		)
	}

	return tabW.Flush()
}

// makePathAbbrev tries to abbreviate the `dst` path if
// both are in the same directory.
func makePathAbbrev(src, dst string) string {
	if path.Dir(src) == path.Dir(dst) {
		return path.Base(dst)
	}

	relPath, err := filepath.Rel(path.Dir(src), dst)
	if err != nil {
		fmt.Println("Failed to get relatie path: ", err)
		return dst
	}

	// We could also possibly check here if relPath is longer than dst
	// and only display the relative version then. But being consistent
	// is more valueable here I think.
	return relPath
}

func printDiffTree(diff *client.Diff) {
	const (
		diffTypeNone = iota
		diffTypeAdded
		diffTypeRemoved
		diffTypeMoved
		diffTypeIgnored
		diffTypeConflict
		diffTypeMerged
	)

	type diffEntry struct {
		typ  int
		pair client.DiffPair
	}

	entries := []client.StatInfo{}
	types := make(map[string]diffEntry)

	// Singular types:
	for _, info := range diff.Added {
		types[info.Path] = diffEntry{typ: diffTypeAdded}
		entries = append(entries, info)
	}
	for _, info := range diff.Removed {
		types[info.Path] = diffEntry{typ: diffTypeRemoved}
		entries = append(entries, info)
	}
	for _, info := range diff.Ignored {
		types[info.Path] = diffEntry{typ: diffTypeIgnored}
		entries = append(entries, info)
	}

	// Pair types:
	for _, pair := range diff.Moved {
		types[pair.Dst.Path] = diffEntry{
			typ:  diffTypeMoved,
			pair: pair,
		}
		entries = append(entries, pair.Dst)
	}
	for _, pair := range diff.Conflict {
		types[pair.Dst.Path] = diffEntry{
			typ:  diffTypeConflict,
			pair: pair,
		}
		entries = append(entries, pair.Dst)
	}
	for _, pair := range diff.Merged {
		types[pair.Dst.Path] = diffEntry{
			typ:  diffTypeMerged,
			pair: pair,
		}
		entries = append(entries, pair.Dst)
	}

	if len(entries) == 0 {
		// Nothing to show:
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	// Called to format each name in the resulting tree:
	formatter := func(n *treeNode) string {
		if n.name == "/" {
			return color.MagentaString("•")
		}

		if diffEntry, ok := types[n.entry.Path]; ok {
			switch diffEntry.typ {
			case diffTypeAdded:
				return color.GreenString(" + " + n.name)
			case diffTypeRemoved:
				return color.RedString(" - " + n.name)
			case diffTypeIgnored:
				return color.YellowString(" * " + n.name)
			case diffTypeMoved:
				// TODO: Print base(src) and relTo(src, dst)
				dstPath := makePathAbbrev(
					diffEntry.pair.Src.Path,
					diffEntry.pair.Dst.Path,
				)
				name := fmt.Sprintf(
					" %s → %s",
					path.Base(diffEntry.pair.Src.Path),
					dstPath,
				)
				return color.BlueString(name)
			case diffTypeMerged:
				dstPath := makePathAbbrev(
					diffEntry.pair.Src.Path,
					diffEntry.pair.Dst.Path,
				)
				name := fmt.Sprintf(
					" %s ⇄ %s",
					path.Base(diffEntry.pair.Src.Path),
					dstPath,
				)
				return color.CyanString(name)
			case diffTypeConflict:
				dstPath := makePathAbbrev(
					diffEntry.pair.Src.Path,
					diffEntry.pair.Dst.Path,
				)
				name := fmt.Sprintf(
					" %s ⚡ %s",
					path.Base(diffEntry.pair.Src.Path),
					dstPath,
				)
				return color.MagentaString(name)
			}
		}

		return n.name
	}

	// Render the tree:
	showTree(entries, &treeCfg{
		format:  formatter,
		showPin: false,
	})
}

func printDiff(diff *client.Diff) {
	simpleSection := func(heading string, infos []client.StatInfo) {
		if len(infos) == 0 {
			return
		}

		fmt.Println(heading)
		for _, info := range infos {
			fmt.Printf("  %s\n", info.Path)
		}

		fmt.Println()
	}

	pairSection := func(heading string, infos []client.DiffPair) {
		if len(infos) == 0 {
			return
		}

		fmt.Println(heading)
		for _, pair := range infos {
			fmt.Printf("  %s <-> %s\n", pair.Src.Path, pair.Dst.Path)
		}

		fmt.Println()
	}

	simpleSection(color.GreenString("Added:"), diff.Added)
	simpleSection(color.YellowString("Ignored:"), diff.Ignored)
	simpleSection(color.RedString("Removed:"), diff.Removed)

	pairSection(color.BlueString("Moved:"), diff.Moved)
	pairSection(color.CyanString("Resolveable Conflicts:"), diff.Merged)
	pairSection(color.MagentaString("Conflicts:"), diff.Conflict)
}

func handleDiff(ctx *cli.Context, ctl *client.Client) error {
	if ctx.NArg() > 4 {
		fmt.Println("More than four arguments can't be handled.")
	}

	self, err := ctl.Whoami()
	if err != nil {
		return err
	}

	localName := self.CurrentUser
	remoteName := self.CurrentUser

	localRev := "HEAD"
	remoteRev := "CURR"

	switch n := ctx.NArg(); n {
	case 1:
		remoteRev = ctx.Args().Get(0)
	case 2:
		localRev = ctx.Args().Get(0)
		remoteRev = ctx.Args().Get(1)
	case 3:
		remoteName = ctx.Args().Get(0)
		localRev = ctx.Args().Get(1)
		remoteRev = ctx.Args().Get(2)
	case 4:
		localName = ctx.Args().Get(0)
		remoteName = ctx.Args().Get(1)
		localRev = ctx.Args().Get(2)
		remoteRev = ctx.Args().Get(3)
	}

	diff, err := ctl.MakeDiff(localName, remoteName, localRev, remoteRev)
	if err != nil {
		return ExitCode{UnknownError, fmt.Sprintf("diff: %v", err)}
	}

	if ctx.Bool("list") {
		printDiff(diff)
	} else {
		printDiffTree(diff)
	}

	return nil
}

func handleFetch(ctx *cli.Context, ctl *client.Client) error {
	who := ctx.Args().First()
	return ctl.Fetch(who)
}

func handleSync(ctx *cli.Context, ctl *client.Client) error {
	who := ctx.Args().First()

	needFetch := true
	if ctx.Bool("no-fetch") {
		needFetch = false
	}

	return ctl.Sync(who, needFetch)
}

func handleStatus(ctx *cli.Context, ctl *client.Client) error {
	self, err := ctl.Whoami()
	if err != nil {
		return err
	}

	curr := self.CurrentUser
	diff, err := ctl.MakeDiff(curr, curr, "HEAD", "CURR")
	if err != nil {
		return err
	}

	if ctx.Bool("tree") {
		printDiffTree(diff)
	} else {
		printDiff(diff)
	}

	return nil
}

func handleBecome(ctx *cli.Context, ctl *client.Client) error {
	becomeSelf := ctx.Bool("self")
	if !becomeSelf && ctx.NArg() < 1 {
		return fmt.Errorf("become needs at least one argument without -s")
	}

	whoami, err := ctl.Whoami()
	if err != nil {
		return err
	}

	who := ctx.Args().First()
	if becomeSelf {
		who = whoami.Owner
	}

	if whoami.CurrentUser == who {
		fmt.Printf("You are already %s.\n", color.GreenString(who))
		return nil
	}

	if err := ctl.Become(who); err != nil {
		return err
	}

	suffix := "Changes will be local only."
	if who == whoami.Owner {
		suffix = "Welcome back!"
	}

	fmt.Printf(
		"You are viewing %s's data now. %s\n",
		color.GreenString(who), suffix,
	)
	return nil
}

func handleCommit(ctx *cli.Context, ctl *client.Client) error {
	msg := ""
	if msg = ctx.String("message"); msg == "" {
		msg = fmt.Sprintf("manual commit")
	}

	if err := ctl.MakeCommit(msg); err != nil {
		return ExitCode{UnknownError, fmt.Sprintf("commit: %v", err)}
	}

	return nil
}

func handleTag(ctx *cli.Context, ctl *client.Client) error {
	if ctx.Bool("delete") {
		name := ctx.Args().Get(0)

		if err := ctl.Untag(name); err != nil {
			return ExitCode{
				UnknownError,
				fmt.Sprintf("untag: %v", err),
			}
		}
	} else {
		if len(ctx.Args()) < 2 {
			return ExitCode{BadArgs, "tag needs at least two arguments"}
		}

		rev := ctx.Args().Get(0)
		name := ctx.Args().Get(1)

		if err := ctl.Tag(rev, name); err != nil {
			return ExitCode{
				UnknownError,
				fmt.Sprintf("tag: %v", err),
			}
		}
	}

	return nil
}

func handleLog(ctx *cli.Context, ctl *client.Client) error {
	entries, err := ctl.Log()
	if err != nil {
		return ExitCode{UnknownError, fmt.Sprintf("commit: %v", err)}
	}

	for _, entry := range entries {
		tags := ""
		if len(entry.Tags) > 0 {
			tags = fmt.Sprintf(" (%s)", strings.Join(entry.Tags, ", "))
		}

		msg := entry.Msg
		if msg == "" {
			msg = color.RedString("•")
		}

		entry.Hash.ShortB58()

		fmt.Printf(
			"%s %s %s%s\n",
			color.GreenString(entry.Hash.ShortB58()),
			color.YellowString(entry.Date.Format(time.Stamp)),
			msg,
			color.CyanString(tags),
		)
	}

	return nil
}
