package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cbluth/bittorrent/pkg/cli"
)

// Operations provides config file CRUD operations
type Operations struct {
	configPath string
}

// NewOperations creates a new config operations instance using the default path.
func NewOperations() *Operations {
	return &Operations{
		configPath: GetConfigPath(),
	}
}

// NewOperationsWithPath creates a config operations instance with a custom config file path.
func NewOperationsWithPath(path string) *Operations {
	return &Operations{
		configPath: path,
	}
}

// load loads the config file, creating default if it doesn't exist
func (o *Operations) load() (map[string]interface{}, error) {
	// Create config file with defaults if it doesn't exist
	if _, err := os.Stat(o.configPath); os.IsNotExist(err) {
		defaultConfig := map[string]interface{}{
			"version": 1,
			"node_id": "",
			"dht": map[string]interface{}{
				"nodes": []string{},
				"feed":  []string{},
			},
			"trackers": map[string]interface{}{
				"list": []string{},
				"feed": []string{},
			},
			"stun": map[string]interface{}{
				"servers": []string{},
				"feed":    []string{},
			},
		}

		if err := o.save(defaultConfig); err != nil {
			return nil, fmt.Errorf("failed to create default config: %w", err)
		}
		return defaultConfig, nil
	}

	data, err := os.ReadFile(o.configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return config, nil
}

// GetNodeID retrieves the node ID from config
func (o *Operations) GetNodeID() (string, error) {
	config, err := o.load()
	if err != nil {
		return "", err
	}

	if nodeID, ok := config["node_id"].(string); ok {
		return nodeID, nil
	}
	return "", nil
}

// GetShareRatio returns the configured share ratio.
// Returns math.Inf(1) (seed forever) when not set or invalid.
func (o *Operations) GetShareRatio() float64 {
	config, err := o.load()
	if err != nil {
		return math.Inf(1)
	}
	if v, ok := config["share_ratio"].(float64); ok {
		return v
	}
	return math.Inf(1)
}

// SetShareRatio saves the share ratio to config.
// Use math.Inf(1) to seed forever, 0.0 to stop immediately after 100%.
func (o *Operations) SetShareRatio(ratio float64) error {
	config, err := o.load()
	if err != nil {
		return err
	}
	if math.IsInf(ratio, 1) {
		// Store as null / delete key so it reads back as "seed forever"
		delete(config, "share_ratio")
	} else {
		config["share_ratio"] = ratio
	}
	return o.save(config)
}

// SetNodeID saves the node ID to config
func (o *Operations) SetNodeID(nodeID string) error {
	config, err := o.load()
	if err != nil {
		return err
	}

	config["node_id"] = nodeID
	return o.save(config)
}

// save saves the config file
func (o *Operations) save(config map[string]interface{}) error {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(o.configPath), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(o.configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

// getNestedValue retrieves a value from nested map using dot notation
func getNestedValue(data map[string]interface{}, key string) (interface{}, bool) {
	parts := strings.Split(key, ".")
	current := data

	for i, part := range parts {
		val, ok := current[part]
		if !ok {
			return nil, false
		}

		if i == len(parts)-1 {
			return val, true
		}

		// Navigate deeper
		nextMap, ok := val.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = nextMap
	}

	return nil, false
}

// setNestedValue sets a value in nested map using dot notation
func setNestedValue(data map[string]interface{}, key string, value interface{}) {
	parts := strings.Split(key, ".")
	current := data

	for i, part := range parts {
		if i == len(parts)-1 {
			// Set the value
			current[part] = value
			return
		}

		// Navigate or create nested map
		if val, ok := current[part]; ok {
			if nextMap, ok := val.(map[string]interface{}); ok {
				current = nextMap
			} else {
				// Replace non-map with map
				newMap := make(map[string]interface{})
				current[part] = newMap
				current = newMap
			}
		} else {
			// Create new nested map
			newMap := make(map[string]interface{})
			current[part] = newMap
			current = newMap
		}
	}
}

// deleteNestedValue deletes a value in nested map using dot notation
func deleteNestedValue(data map[string]interface{}, key string) bool {
	parts := strings.Split(key, ".")
	current := data

	for i, part := range parts {
		val, ok := current[part]
		if !ok {
			return false
		}

		if i == len(parts)-1 {
			// Delete the key
			delete(current, part)
			return true
		}

		// Navigate deeper
		nextMap, ok := val.(map[string]interface{})
		if !ok {
			return false
		}
		current = nextMap
	}

	return false
}

// parseValue tries to parse a string value as JSON, falling back to string
func parseValue(s string) interface{} {
	// Try to parse as JSON
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	// Fall back to string
	return s
}

// Get retrieves a config value by key (dot notation supported)
func (o *Operations) Get(key string) (interface{}, error) {
	config, err := o.load()
	if err != nil {
		return nil, err
	}

	value, ok := getNestedValue(config, key)
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}

	return value, nil
}

// Set sets a config value by key (dot notation supported)
func (o *Operations) Set(key string, valueStr string) error {
	config, err := o.load()
	if err != nil {
		return err
	}

	value := parseValue(valueStr)
	setNestedValue(config, key, value)

	return o.save(config)
}

// Delete deletes a config value by key (dot notation supported)
func (o *Operations) Delete(key string) error {
	config, err := o.load()
	if err != nil {
		return err
	}

	if !deleteNestedValue(config, key) {
		return fmt.Errorf("key not found: %s", key)
	}

	return o.save(config)
}

// List returns all config values
func (o *Operations) List() (map[string]interface{}, error) {
	return o.load()
}

// Edit opens the config file in the user's editor
func (o *Operations) Edit() error {
	// Ensure config exists
	if _, err := o.load(); err != nil {
		return err
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi" // Default to vi
	}

	cmd := exec.Command(editor, o.configPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to open editor: %w", err)
	}

	// Validate the edited config
	if _, err := o.load(); err != nil {
		return fmt.Errorf("config file is invalid after editing: %w", err)
	}

	return nil
}

// GetLogTopics returns the configured log topic filter list.
// Returns nil when the "log" key is absent (meaning: log everything).
func (o *Operations) GetLogTopics() []string {
	config, err := o.load()
	if err != nil {
		return nil
	}
	raw, ok := config["log"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	topics := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			topics = append(topics, s)
		}
	}
	return topics
}

// Path returns the config file path
func (o *Operations) Path() string {
	return o.configPath
}

// Run handles config commands with routing
func Run(args []string) error {
	router := cli.NewRouter("bt config")
	router.RegisterCommand(&cli.Command{
		Run:  CmdGet,
		Name: "get", Aliases: []string{"g"},
		Description: "Get value of a configuration key",
	})
	router.RegisterCommand(&cli.Command{
		Run:  CmdSet,
		Name: "set", Aliases: []string{"s"},
		Description: "Set value of a configuration key",
	})
	router.RegisterCommand(&cli.Command{
		Run:  CmdAppend,
		Name: "append", Aliases: []string{"add", "a"},
		Description: "Append a value to an array configuration key",
	})
	router.RegisterCommand(&cli.Command{
		Run:         CmdDel,
		Description: "Delete a configuration key",
		Name:        "del", Aliases: []string{"delete", "rm"},
	})
	router.RegisterCommand(&cli.Command{
		Run:         CmdList,
		Description: "List all configuration values",
		Name:        "list", Aliases: []string{"ls", "show"},
	})
	router.RegisterCommand(&cli.Command{
		Run:  CmdEdit,
		Name: "edit", Aliases: []string{"e"},
		Description: "Open configuration file in $EDITOR",
	})

	// Route to the appropriate command handler
	return router.Route(args)
}

// Append appends a string value to an array-valued config key.
// If the key does not exist it is created as a single-element array.
// If the key exists but is not an array, an error is returned.
// Duplicate values are ignored.
func (o *Operations) Append(key string, value string) error {
	config, err := o.load()
	if err != nil {
		return err
	}

	existing, ok := getNestedValue(config, key)
	var arr []interface{}
	if ok {
		arr, ok = existing.([]interface{})
		if !ok {
			return fmt.Errorf("key %q is not an array", key)
		}
		// deduplicate
		for _, v := range arr {
			if s, ok := v.(string); ok && s == value {
				return nil // already present
			}
		}
	}
	arr = append(arr, value)
	setNestedValue(config, key, arr)
	return o.save(config)
}

// CmdAppend handles the append command
func CmdAppend(args []string) error {
	f := cli.NewCommandFlagSet(
		"append", []string{"add", "a"},
		"Append a value to an array configuration key",
		[]string{"bt config append <key> <value>"},
	)

	if err := f.Parse(args); err != nil {
		return err
	}
	args = f.Args()

	if len(args) < 2 {
		f.Usage()
		return fmt.Errorf("missing key or value argument")
	}

	ops := NewOperations()
	if err := ops.Append(args[0], args[1]); err != nil {
		return err
	}

	fmt.Printf("Appended to %s\n", args[0])
	return nil
}

// CmdGet handles the get command
func CmdGet(args []string) error {
	f := cli.NewCommandFlagSet(
		"get", []string{"g"},
		"Get value of a configuration key",
		[]string{"bt config get <key>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing key argument")
	}

	ops := NewOperations()
	value, err := ops.Get(args[0])
	if err != nil {
		return err
	}

	// Pretty print the value
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(data))
	return nil
}

// CmdSet handles the set command
func CmdSet(args []string) error {
	f := cli.NewCommandFlagSet(
		"set", []string{"s"},
		"Set value of a configuration key",
		[]string{"bt config set <key> <value>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required arguments
	if len(args) < 2 {
		f.Usage()
		return fmt.Errorf("missing key or value argument")
	}

	ops := NewOperations()
	if err := ops.Set(args[0], args[1]); err != nil {
		return err
	}

	fmt.Printf("Set %s\n", args[0])
	return nil
}

// CmdDel handles the delete command
func CmdDel(args []string) error {
	f := cli.NewCommandFlagSet(
		"del", []string{"delete", "rm"},
		"Delete a configuration key",
		[]string{"bt config del <key>"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Check for required argument
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing key argument")
	}

	ops := NewOperations()
	if err := ops.Delete(args[0]); err != nil {
		return err
	}

	fmt.Printf("Deleted %s\n", args[0])
	return nil
}

// CmdList handles the list command
func CmdList(args []string) error {
	f := cli.NewCommandFlagSet(
		"list", []string{"ls", "show"},
		"List all configuration values",
		[]string{"bt config list"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}

	ops := NewOperations()
	cfg, err := ops.List()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(data))
	return nil
}

// CmdEdit handles the edit command
func CmdEdit(args []string) error {
	f := cli.NewCommandFlagSet(
		"edit", []string{"e"},
		"Open configuration file in $EDITOR",
		[]string{"bt config edit"},
	)

	err := f.Parse(args)
	if err != nil {
		return err
	}

	ops := NewOperations()
	if err := ops.Edit(); err != nil {
		return err
	}

	fmt.Println("Config saved successfully")
	return nil
}
