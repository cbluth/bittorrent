package cli_test

import (
	"fmt"
	"os"

	"github.com/cbluth/bittorrent/pkg/cli"
)

func ExampleCLI() {
	app := cli.New("myapp", "a useful tool", "https://example.com")

	// Global flags
	var configDir string
	app.FlagSet().StringVar(&configDir, "c", "/etc/myapp", "config directory")

	// Initialization hook
	app.SetPreRun(func() error {
		fmt.Fprintf(os.Stderr, "loading config from %s\n", configDir)
		return nil
	})

	// Register commands
	app.RegisterCommand(&cli.Command{
		Name:        "serve",
		Aliases:     []string{"server", "s"},
		Description: "start the HTTP server",
		Category:    "Server Commands",
		Run: func(args []string) error {
			fmt.Println("server started")
			return nil
		},
	})

	app.RegisterCommand(&cli.Command{
		Name:        "add",
		Aliases:     []string{"put", "push"},
		Description: "add a resource",
		Category:    "Client Commands",
		Run: func(args []string) error {
			fmt.Println("added:", args)
			return nil
		},
	})

	// Run with "add" command
	app.Run([]string{"add", "file.txt"})
	// Output: added: [file.txt]
}

func ExampleCLI_help() {
	app := cli.New("myapp", "a useful tool", "")
	app.FlagSet().SetOutput(os.Stdout)

	app.RegisterCommand(&cli.Command{
		Name:        "serve",
		Description: "start the server",
		Category:    "Server Commands",
		Run:         func(args []string) error { return nil },
	})

	app.RegisterCommand(&cli.Command{
		Name:        "get",
		Aliases:     []string{"fetch"},
		Description: "fetch a resource",
		Category:    "Client Commands",
		Run:         func(args []string) error { return nil },
	})

	// No args shows help and returns nil
	err := app.Run([]string{})
	fmt.Println("error:", err)
	// Output:
	//
	// myapp | a useful tool
	//
	// Server Commands:
	//   serve  start the server
	//
	// Client Commands:
	//   get  fetch a resource
	//
	// Usage:
	//   myapp [options] <command>
	//   myapp [options] <command> -h
	//
	// Options:
	// error: <nil>
}

func ExampleRouter() {
	cli.New("myapp", "a useful tool", "") // sets package-level default
	router := cli.NewRouter("myapp")

	router.RegisterCommand(&cli.Command{
		Name:        "get",
		Aliases:     []string{"fetch", "pull"},
		Description: "Get an item",
		Run: func(args []string) error {
			fmt.Println("get:", args)
			return nil
		},
	})

	router.RegisterCommand(&cli.Command{
		Name:        "add",
		Aliases:     []string{"put"},
		Description: "Add an item",
		Run: func(args []string) error {
			fmt.Println("add:", args)
			return nil
		},
	})

	router.Route([]string{"fetch", "item-1"})
	router.Route([]string{"put", "item-2"})
	// Output:
	// get: [item-1]
	// add: [item-2]
}

func ExampleMakeUsage() {
	cli.New("myapp", "a useful tool", "https://example.com") // sets package-level default
	f := cli.NewCommandFlagSet(
		"add", []string{"add", "put", "push"},
		"add a resource", []string{"myapp add [options] <file>"},
	)
	f.SetOutput(os.Stdout)
	f.String("hash", "sha256", "hash algorithm")

	// Trigger usage output
	f.Usage()
	// Output:
	//
	// myapp | a useful tool
	//
	//  Find more information at: https://example.com
	//
	// Command:
	//   add             add a resource
	//
	// Aliases:
	//   add, put, push
	//
	// Usage:
	//   myapp add [options] <file>
	//
	// Options:
	//   -hash string
	//     	hash algorithm (default "sha256")
}

func ExampleParseAndRequire() {
	cli.New("myapp", "a useful tool", "") // sets package-level default
	f := cli.NewCommandFlagSet(
		"get", nil,
		"fetch a resource", nil,
	)
	f.SetOutput(os.Stdout)

	remaining, err := cli.ParseAndRequire(f, []string{"item-1", "item-2"}, 1, "")
	fmt.Println("remaining:", remaining)
	fmt.Println("error:", err)
	// Output:
	// remaining: [item-1 item-2]
	// error: <nil>
}
