package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Command represents a CLI command with its metadata and handler.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	UsageLines  []string                  // Optional: for help text generation
	Category    string                    // Optional: e.g., "Client Commands", "Server Commands"
	Run         func(args []string) error // Optional: handler function
	SkipPreRun  bool                      // If true, preRun is not called for this command

	// Completion hints — consumed by the completion generator.
	CompletionWords []string // static word list for positional arg completion
	CompletionFile  string   // file glob for _filedir, e.g., "@(json|yaml)"
	NoArgCompletion bool     // disable arg completion entirely
}

// AppInfo holds top-level application metadata for consistent help output.
// Set once via New(); inherited automatically by NewRouter and NewCommandFlagSet.
type AppInfo struct {
	Name        string
	Description string
	URL         string
}

// defaultApp is the package-level default set by New().
// Same pattern as flag.CommandLine — one CLI app per process.
var defaultApp AppInfo

// CLI manages the root command-line interface.
type CLI struct {
	app        AppInfo
	flagSet    *flag.FlagSet
	commands   map[string]*Command // name/alias -> command
	ordered    []*Command          // insertion order, deduplicated
	categories []string            // ordered list of categories
	preRun     func() error
}

// UsageConfig holds configuration for generating usage text.
type UsageConfig struct {
	AppName     string
	AppDesc     string
	URL         string
	CommandName string
	CommandDesc string
	Aliases     []string
	UsageLines  []string
}

// New creates a new CLI instance and sets the package-level default AppInfo.
func New(name, description, url string) *CLI {
	defaultApp = AppInfo{Name: name, Description: description, URL: url}
	c := &CLI{
		app:      defaultApp,
		commands: make(map[string]*Command),
	}
	c.flagSet = flag.NewFlagSet(name, flag.ContinueOnError)
	c.flagSet.Usage = c.usage
	return c
}

// FlagSet returns the root flag set for registering global flags.
func (c *CLI) FlagSet() *flag.FlagSet {
	return c.flagSet
}

// RegisterCommand registers a command and its aliases.
func (c *CLI) RegisterCommand(cmd *Command) {
	c.commands[cmd.Name] = cmd
	for _, alias := range cmd.Aliases {
		c.commands[alias] = cmd
	}
	if !slices.Contains(c.ordered, cmd) {
		c.ordered = append(c.ordered, cmd)
	}
	if cmd.Category != "" && !slices.Contains(c.categories, cmd.Category) {
		c.categories = append(c.categories, cmd.Category)
	}
}

// SetPreRun sets a function to run before any command executes.
func (c *CLI) SetPreRun(fn func() error) {
	c.preRun = fn
}

// Run parses arguments and executes the appropriate command.
func (c *CLI) Run(args []string) error {
	if err := c.flagSet.Parse(args); err != nil {
		return err
	}

	cmdArgs := c.flagSet.Args()
	if len(cmdArgs) == 0 {
		c.flagSet.Usage()
		return nil
	}

	cmdName := cmdArgs[0]

	if cmdName == "help" || cmdName == "-h" || cmdName == "-help" {
		c.flagSet.Usage()
		return nil
	}

	cmd, ok := c.commands[cmdName]
	if !ok {
		return fmt.Errorf("unknown command: %s", cmdName)
	}

	if c.preRun != nil && !cmd.SkipPreRun {
		if err := c.preRun(); err != nil {
			return err
		}
	}

	return cmd.Run(cmdArgs[1:])
}

// usage generates and prints the help text.
func (c *CLI) usage() {
	w := c.flagSet.Output()
	fmt.Fprintf(w, "\n%s | %s\n\n", c.app.Name, c.app.Description)

	if c.app.URL != "" {
		fmt.Fprintf(w, " Find more information at: %s\n\n", c.app.URL)
	}

	// Group commands by category using insertion order.
	// Commands with an empty Category are held separately so the fallback
	// "Commands" section never duplicates an explicit Category: "Commands" group.
	categoryCommands := make(map[string][]*Command)
	var uncategorized []*Command
	for _, cmd := range c.ordered {
		if cmd.Category == "" {
			uncategorized = append(uncategorized, cmd)
		} else {
			categoryCommands[cmd.Category] = append(categoryCommands[cmd.Category], cmd)
		}
	}

	for _, category := range c.categories {
		cmds := categoryCommands[category]
		if len(cmds) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s:\n", category)
		c.printCommands(w, cmds)
		fmt.Fprintln(w)
	}

	// Print truly uncategorized commands under a generic "Commands" header.
	if len(uncategorized) > 0 {
		fmt.Fprintln(w, "Commands:")
		c.printCommands(w, uncategorized)
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "Usage:\n")
	fmt.Fprintf(w, "  %s [options] <command>\n", c.app.Name)
	fmt.Fprintf(w, "  %s [options] <command> -h\n\n", c.app.Name)
	fmt.Fprintf(w, "Options:\n")
	c.flagSet.PrintDefaults()
}

// printCommands prints a list of commands with consistent formatting.
func (c *CLI) printCommands(w io.Writer, cmds []*Command) {
	maxLen := 0
	for _, cmd := range cmds {
		if len(cmd.Name) > maxLen {
			maxLen = len(cmd.Name)
		}
	}
	for _, cmd := range cmds {
		padding := strings.Repeat(" ", maxLen-len(cmd.Name))
		fmt.Fprintf(w, "  %s%s  %s\n", cmd.Name, padding, cmd.Description)
	}
}

// MakeUsage creates a usage function for a FlagSet with consistent formatting.
func MakeUsage(cfg UsageConfig, f *flag.FlagSet) func() {
	return func() {
		w := f.Output()
		fmt.Fprintf(w, "\n%s | %s\n\n", cfg.AppName, cfg.AppDesc)

		if cfg.URL != "" {
			fmt.Fprintf(w, " Find more information at: %s\n\n", cfg.URL)
		}

		if cfg.CommandName != "" {
			fmt.Fprintf(w, "Command:\n")
			fmt.Fprintf(w, "  %-15s %s\n\n", cfg.CommandName, cfg.CommandDesc)
		}

		if len(cfg.Aliases) > 0 {
			fmt.Fprintf(w, "Aliases:\n")
			fmt.Fprintf(w, "  %s\n\n", strings.Join(cfg.Aliases, ", "))
		}

		if len(cfg.UsageLines) > 0 {
			fmt.Fprintf(w, "Usage:\n")
			for _, line := range cfg.UsageLines {
				fmt.Fprintf(w, "  %s\n", line)
			}
			fmt.Fprintln(w)
		}

		fmt.Fprintf(w, "Options:\n")
		f.PrintDefaults()
	}
}

// Router provides command routing with alias support and auto-generated help.
type Router struct {
	name     string // parent command name, e.g. "bt dht"
	app      AppInfo
	routes   map[string]func([]string) error
	commands map[string]*Command
	ordered  []*Command // insertion order for help output
}

// NewRouter creates a command router for a subcommand group.
// App info is inherited from the package-level default set by New().
func NewRouter(name string) *Router {
	return &Router{
		name:     name,
		app:      defaultApp,
		routes:   make(map[string]func([]string) error),
		commands: make(map[string]*Command),
	}
}

// RegisterCommand registers a command with full metadata.
// Always use this instead of adding raw handlers — it populates the
// auto-generated help output with the command's name, aliases, and description.
func (r *Router) RegisterCommand(cmd *Command) {
	r.commands[cmd.Name] = cmd
	r.routes[cmd.Name] = cmd.Run
	for _, alias := range cmd.Aliases {
		r.routes[alias] = cmd.Run
	}
	if !slices.Contains(r.ordered, cmd) {
		r.ordered = append(r.ordered, cmd)
	}
}

// Route finds and executes the handler for a command.
func (r *Router) Route(args []string) error {
	if len(args) == 0 {
		r.usage()
		return nil
	}

	cmd := args[0]
	if cmd == "help" || cmd == "-h" || cmd == "-help" || cmd == "--help" {
		r.usage()
		return nil
	}

	handler, ok := r.routes[cmd]
	if !ok {
		return fmt.Errorf("unknown command: %s", cmd)
	}

	return handler(args[1:])
}

// usage prints auto-generated help from registered commands.
func (r *Router) usage() {
	w := flag.CommandLine.Output()
	name := r.name
	if name == "" {
		name = "command"
	}

	if len(r.ordered) == 0 {
		fmt.Fprintf(w, "%s: no subcommands registered\n", name)
		return
	}

	if r.app.Name != "" {
		fmt.Fprintf(w, "\n%s | %s\n\n", r.app.Name, r.app.Description)
		if r.app.URL != "" {
			fmt.Fprintf(w, " Find more information at: %s\n\n", r.app.URL)
		}
	}

	fmt.Fprintf(w, "Subcommands:\n")
	maxLen := 0
	for _, cmd := range r.ordered {
		if len(cmd.Name) > maxLen {
			maxLen = len(cmd.Name)
		}
	}
	for _, cmd := range r.ordered {
		aliases := ""
		if len(cmd.Aliases) > 0 {
			aliases = "  (" + strings.Join(cmd.Aliases, ", ") + ")"
		}
		padding := strings.Repeat(" ", maxLen-len(cmd.Name))
		fmt.Fprintf(w, "  %s%s  %s%s\n", cmd.Name, padding, cmd.Description, aliases)
	}
	fmt.Fprintf(w, "\nUsage:\n  %s <subcommand> [options]\n  %s <subcommand> -h\n", name, name)
}

// NewCommandFlagSet creates a FlagSet with consistent usage formatting.
// App info is inherited from the package-level default set by New().
func NewCommandFlagSet(
	commandName string, aliases []string,
	description string, usageLines []string,
) *flag.FlagSet {
	f := flag.NewFlagSet(commandName, flag.ContinueOnError)
	f.Usage = MakeUsage(UsageConfig{
		AppName:     defaultApp.Name,
		AppDesc:     defaultApp.Description,
		URL:         defaultApp.URL,
		CommandName: commandName,
		CommandDesc: description,
		Aliases:     aliases,
		UsageLines:  usageLines,
	}, f)
	return f
}

// CommandFlagSet creates a FlagSet for a command under this router,
// with consistent usage formatting inheriting the router's app info.
func (r *Router) CommandFlagSet(
	commandName string, aliases []string,
	description string, usageLines []string,
) *flag.FlagSet {
	f := flag.NewFlagSet(commandName, flag.ContinueOnError)
	f.Usage = MakeUsage(UsageConfig{
		AppName:     r.app.Name,
		AppDesc:     r.app.Description,
		URL:         r.app.URL,
		CommandName: commandName,
		CommandDesc: description,
		Aliases:     aliases,
		UsageLines:  usageLines,
	}, f)
	return f
}

// ParseArgs parses flags and returns remaining arguments.
func ParseArgs(f *flag.FlagSet, args []string) ([]string, error) {
	if err := f.Parse(args); err != nil {
		return nil, err
	}
	return f.Args(), nil
}

// ParseAndRequire parses flags and ensures at least minArgs remain.
// Shows usage and returns error if not enough args provided.
func ParseAndRequire(f *flag.FlagSet, args []string, minArgs int, errorMsg string) ([]string, error) {
	remaining, err := ParseArgs(f, args)
	if err != nil {
		return nil, err
	}

	if len(remaining) < minArgs {
		f.Usage()
		if errorMsg == "" {
			errorMsg = "not enough arguments"
		}
		return nil, errors.New(errorMsg)
	}

	return remaining, nil
}

// GetCommands returns all registered commands in insertion order (deduplicated).
func (c *CLI) GetCommands() []*Command {
	return c.ordered
}

// GetFlagNames returns all global flag names.
func (c *CLI) GetFlagNames() []string {
	var flags []string
	c.flagSet.VisitAll(func(f *flag.Flag) {
		flags = append(flags, f.Name)
	})
	return flags
}

// GetCategories returns the list of categories in order.
func (c *CLI) GetCategories() []string {
	return c.categories
}
