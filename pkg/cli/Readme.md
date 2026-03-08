# cli

A portable, zero-dependency CLI framework for Go. Copy this directory into any
project and change the import path.

## Quick Start

```go
app := cli.New("myapp", "does things", "https://example.com")

app.FlagSet().StringVar(&cfgDir, "c", "/etc/myapp", "config directory")

app.RegisterCommand(&cli.Command{
    Name:        "serve",
    Aliases:     []string{"server", "s"},
    Description: "start the server",
    Category:    "Server Commands",
    Run:         runServer,
})

app.Run(os.Args[1:])
```

Running with no arguments prints help and returns nil.

## Command Registration

```go
app.RegisterCommand(&cli.Command{
    Name:        "add",
    Aliases:     []string{"put", "push"},
    Description: "add a resource",
    Category:    "Client Commands",
    Run:         runAdd,

    // Completion hints (optional — used by the completion generator)
    CompletionFile:  "@(json|yaml)",                    // _filedir glob
    CompletionWords: []string{"sha256", "sha512"},     // static word list
    NoArgCompletion: true,                             // suppress arg completion
})
```

Commands are printed in insertion order under their category headers.

## PreRun Hook

```go
app.SetPreRun(func() error {
    return config.Open(cfgDir)
})
```

Runs before every command except `completion`.

## Help Formatting

`MakeUsage` creates a consistent usage function for any `flag.FlagSet`:

```go
f := flag.NewFlagSet("add", flag.ContinueOnError)
f.Usage = cli.MakeUsage(cli.UsageConfig{
    AppName:     "myapp",
    AppDesc:     "does things",
    URL:         "https://example.com",
    CommandName: "add",
    CommandDesc: "add a resource",
    Aliases:     []string{"add", "put", "push"},
    UsageLines:  []string{"myapp add [options] <file>"},
}, f)
```

`NewCommandFlagSet` is a shorthand that creates the FlagSet and wires up usage
in one call.

## Router

`Router` dispatches to `func([]string) error` handlers by name. Useful for
nesting a second level of commands inside a top-level command handler:

```go
r := cli.NewRouter()
r.Register(runGet, "get", "fetch", "pull")
r.RegisterCommand(&cli.Command{
    Name: "add", Aliases: []string{"put"},
    Run: runAdd,
})
return r.Route(args)
```

## Shell Completion

The `completion` sub-package generates data-driven bash completion scripts.

```go
gen := completion.New("myapp")
gen.AddFlagsFromFlagSet(app.FlagSet())

for _, cmd := range app.GetCommands() {
    gen.AddCommand(completion.CommandInfo{
        Name:            cmd.Name,
        Aliases:         cmd.Aliases,
        CompletionFile:  cmd.CompletionFile,
        CompletionWords: cmd.CompletionWords,
        NoArgCompletion: cmd.NoArgCompletion,
        Flags: []completion.FlagInfo{
            {Name: "hash", TakesValue: true, Values: []string{"sha256", "sha512"}},
        },
    })
}

script, _ := gen.Generate("bash")
fmt.Print(script)
```

Source the output to enable tab completion:

```bash
source <(myapp completion bash)
```

## API Summary

| Type / Function | Purpose |
|---|---|
| `CLI` | Root command manager with global flags |
| `Command` | Command definition with handler and completion hints |
| `Router` | Lightweight name-to-handler dispatch |
| `MakeUsage` | Consistent help text for any FlagSet |
| `NewCommandFlagSet` | FlagSet + MakeUsage in one call |
| `ParseArgs` | Parse flags, return remaining args |
| `ParseAndRequire` | Parse flags, enforce minimum arg count |
| `completion.Generator` | Shell completion script builder |
| `completion.CommandInfo` | Per-command completion metadata |
| `completion.FlagInfo` | Per-flag completion metadata |
