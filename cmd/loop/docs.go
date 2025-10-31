package main

import (
	"context"
	_ "embed"
	"fmt"

	docs "github.com/urfave/cli-docs/v3"
	"github.com/urfave/cli/v3"
)

//go:embed markdown_tabular.md.gotmpl
var markdownTabularDocTemplate string

// We have a copy of this template taken from
// https://github.com/urfave/cli-docs where we remove column
// "Environment variables" if it has no values.
// TODO: remove this when https://github.com/urfave/cli-docs/pull/15
// is merged.
func init() {
	docs.MarkdownTabularDocTemplate = markdownTabularDocTemplate
}

var printManCommand = &cli.Command{
	Name:        "man",
	Usage:       "prints man file",
	Description: "Prints documentation of loop CLI in man format",
	Action:      printMan,
	Hidden:      true,
}

func printMan(_ context.Context, cmd *cli.Command) error {
	root := filterNestedHelpCommands(cmd.Root())

	const userCommandsSection = 1
	man, err := docs.ToManWithSection(root, userCommandsSection)
	if err != nil {
		return fmt.Errorf("failed to produce man: %w", err)
	}

	fmt.Println(man)

	return nil
}

var printMarkdownCommand = &cli.Command{
	Name:        "markdown",
	Usage:       "prints markdown file",
	Description: "Prints documentation of loop CLI in markdown format",
	Action:      printMarkdown,
	Hidden:      true,
}

func printMarkdown(_ context.Context, cmd *cli.Command) error {
	root := filterNestedHelpCommands(cmd.Root())

	md, err := docs.ToTabularMarkdown(root, "loop")
	if err != nil {
		return fmt.Errorf("failed to produce markdown: %w", err)
	}

	fmt.Println(md)

	return nil
}

// filterNestedHelpCommands clones cmd, drops nested help commands, and normalises
// flag defaults so generated documentation avoids absolute paths.
func filterNestedHelpCommands(cmd *cli.Command) *cli.Command {
	cloned := cloneCommand(cmd, 0)
	overrideDocFlags(cloned)
	return cloned
}

// cloneCommand clones the command, filtering out nested "help" subcommands.
func cloneCommand(cmd *cli.Command, depth int) *cli.Command {
	if cmd == nil {
		return nil
	}

	cloned := *cmd
	if len(cmd.Commands) == 0 {
		return &cloned
	}

	filtered := make([]*cli.Command, 0, len(cmd.Commands))
	for _, sub := range cmd.Commands {
		if sub == nil {
			continue
		}
		childDepth := depth + 1

		// TODO: remove when https://github.com/urfave/cli-docs/pull/16
		if childDepth > 0 && sub.Name == "help" {
			continue
		}

		filtered = append(filtered, cloneCommand(sub, childDepth))
	}

	cloned.Commands = filtered
	return &cloned
}

// overrideDocFlags walks the command tree and replaces string flag defaults
// that leak user-specific filesystem paths, keeping generated docs stable.
func overrideDocFlags(cmd *cli.Command) {
	if cmd == nil {
		return
	}

	if len(cmd.Flags) > 0 {
		clonedFlags := make([]cli.Flag, len(cmd.Flags))
		for i, fl := range cmd.Flags {
			clonedFlags[i] = cloneFlagWithOverrides(fl)
		}
		cmd.Flags = clonedFlags
	}

	for _, sub := range cmd.Commands {
		overrideDocFlags(sub)
	}
}

// docFlagOverrides maps global flag names to the canonical values we want to
// show in documentation instead of user-specific absolute paths.
var docFlagOverrides = map[string]string{
	loopDirFlag.Name:      "~/.loop",
	tlsCertFlag.Name:      "~/.loop/mainnet/tls.cert",
	macaroonPathFlag.Name: "~/.loop/mainnet/loop.macaroon",
}

// cloneFlagWithOverrides returns a copy of flag with overridden default values
// when the flag participates in docFlagOverrides. Non-string flags are reused
// unchanged to minimise allocations.
func cloneFlagWithOverrides(flag cli.Flag) cli.Flag {
	sf, ok := flag.(*cli.StringFlag)
	if !ok {
		return flag
	}

	value, ok := docFlagOverrides[sf.Name]
	if !ok {
		return flag
	}

	cloned := *sf
	cloned.Value = value
	cloned.DefaultText = value

	return &cloned
}
