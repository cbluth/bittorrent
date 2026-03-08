package completion

import (
	"errors"
	"flag"
	"fmt"
)

// CommandInfo holds metadata about a command for completion generation.
type CommandInfo struct {
	Name            string
	Aliases         []string
	Category        string
	Flags           []FlagInfo
	CompletionWords []string      // static word list for positional arg completion
	CompletionFile  string        // file glob for _filedir, e.g., "@(json|yaml)"
	NoArgCompletion bool          // disable arg completion entirely
	SubCommands     []CommandInfo // one-level subcommands (e.g., node run|start|stop|status)
}

// FlagInfo holds metadata about a flag for completion.
type FlagInfo struct {
	Name       string
	TakesValue bool     // true for non-bool flags
	Values     []string // possible values for this flag
}

// Generator generates shell completion scripts.
type Generator struct {
	appName     string
	commands    []CommandInfo
	globalFlags []FlagInfo
}

// New creates a new completion generator.
func New(appName string) *Generator {
	return &Generator{
		appName: appName,
	}
}

// AddCommand registers a command for completion.
func (g *Generator) AddCommand(cmd CommandInfo) {
	g.commands = append(g.commands, cmd)
}

// AddGlobalFlag registers a global flag for completion.
func (g *Generator) AddGlobalFlag(fi FlagInfo) {
	g.globalFlags = append(g.globalFlags, fi)
}

// boolFlag is implemented by flag.Value types that are boolean.
type boolFlag interface {
	IsBoolFlag() bool
}

// AddFlagsFromFlagSet extracts flags from a flag.FlagSet and adds them as global flags.
func (g *Generator) AddFlagsFromFlagSet(fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		takesValue := true
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			takesValue = false
		}
		g.AddGlobalFlag(FlagInfo{
			Name:       f.Name,
			TakesValue: takesValue,
		})
	})
}

// Generate generates a completion script for the specified shell.
func (g *Generator) Generate(shell string) (string, error) {
	switch shell {
	case "bash":
		return BashCompletion(g.appName, g.commands, g.globalFlags), nil
	case "zsh":
		return "", errors.New("zsh completion not yet implemented")
	case "fish":
		return "", errors.New("fish completion not yet implemented")
	default:
		return "", fmt.Errorf("unsupported shell: %s (supported: bash, zsh, fish)", shell)
	}
}
