package completion_test

import (
	"fmt"
	"strings"

	"github.com/cbluth/bittorrent/pkg/cli/completion"
)

func ExampleGenerator() {
	gen := completion.New("myapp")

	// Global flags
	gen.AddGlobalFlag(completion.FlagInfo{Name: "config", TakesValue: true})
	gen.AddGlobalFlag(completion.FlagInfo{Name: "verbose"})

	// Commands with completion hints
	gen.AddCommand(completion.CommandInfo{
		Name:    "serve",
		Aliases: []string{"server"},
	})
	gen.AddCommand(completion.CommandInfo{
		Name:           "add",
		Aliases:        []string{"put"},
		CompletionFile: "@(json|yaml)",
		Flags: []completion.FlagInfo{
			{Name: "hash", TakesValue: true, Values: []string{"sha256", "sha512", "blake3"}},
			{Name: "dry-run"},
		},
	})
	gen.AddCommand(completion.CommandInfo{
		Name:            "status",
		CompletionWords: []string{"all", "running", "stopped"},
	})
	gen.AddCommand(completion.CommandInfo{
		Name:            "connect",
		NoArgCompletion: true,
	})

	script, err := gen.Generate("bash")
	if err != nil {
		panic(err)
	}

	// Show key parts of the generated script
	lines := strings.Split(script, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Print structural lines to verify the output
		if strings.HasPrefix(trimmed, "# bash") ||
			strings.HasPrefix(trimmed, "local all_commands") ||
			strings.HasPrefix(trimmed, "local global_flags") ||
			strings.HasPrefix(trimmed, "complete ") {
			fmt.Println(trimmed)
		}
	}
	// Output:
	// # bash completion for myapp | -*- shell-script -*-
	// local all_commands="serve server add put status connect"
	// local global_flags="-config -verbose"
	// complete -F _myapp_completion myapp
}
