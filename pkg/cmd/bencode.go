// pkg/cmd/bencode.go - Bencode CLI commands extracted from pkg/bencode
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/cbluth/bittorrent/pkg/bencode"
	"github.com/cbluth/bittorrent/pkg/cli"
)

// EncodeOptions represents options for encoding
type EncodeOptions struct {
	InputFile  string
	OutputFile string
}

// DecodeOptions represents options for decoding
type DecodeOptions struct {
	InputFile  string
	OutputFile string
	Pretty     bool
	Compact    bool
}

// EncodeFromString encodes a JSON string to bencode
func EncodeFromString(jsonStr string, opts *EncodeOptions) (string, error) {
	// Parse JSON
	var data interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	// Encode to bencode
	encoded, err := bencode.EncodeBytes(data)
	if err != nil {
		return "", fmt.Errorf("bencode encoding failed: %w", err)
	}

	// Write to file if specified
	if opts.OutputFile != "" {
		if err := os.WriteFile(opts.OutputFile, encoded, 0644); err != nil {
			return "", fmt.Errorf("failed to write output file: %w", err)
		}
		return "", nil // No string output when writing to file
	}

	return string(encoded), nil
}

// EncodeFromFile encodes a JSON file to bencode
func EncodeFromFile(inputPath string, opts *EncodeOptions) (string, error) {
	// Read JSON file
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return "", fmt.Errorf("failed to read input file: %w", err)
	}

	// Parse JSON
	var obj interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return "", fmt.Errorf("invalid JSON in file: %w", err)
	}

	// Encode to bencode
	encoded, err := bencode.EncodeBytes(obj)
	if err != nil {
		return "", fmt.Errorf("bencode encoding failed: %w", err)
	}

	// Write to file if specified
	if opts.OutputFile != "" {
		if err := os.WriteFile(opts.OutputFile, encoded, 0644); err != nil {
			return "", fmt.Errorf("failed to write output file: %w", err)
		}
		return "", nil // No string output when writing to file
	}

	return string(encoded), nil
}

// DecodeFromString decodes a bencode string to JSON
func DecodeFromString(bencodeStr string, opts *DecodeOptions) (string, error) {
	// Decode bencode
	var data interface{}
	if err := bencode.DecodeString(bencodeStr, &data); err != nil {
		return "", fmt.Errorf("bencode decoding failed: %w", err)
	}

	// Convert to JSON
	var jsonData []byte
	var err error

	if opts.Pretty {
		jsonData, err = json.MarshalIndent(data, "", "  ")
	} else {
		jsonData, err = json.Marshal(data)
	}

	if err != nil {
		return "", fmt.Errorf("JSON encoding failed: %w", err)
	}

	// Write to file if specified
	if opts.OutputFile != "" {
		if err := os.WriteFile(opts.OutputFile, jsonData, 0644); err != nil {
			return "", fmt.Errorf("failed to write output file: %w", err)
		}
		return "", nil // No string output when writing to file
	}

	return string(jsonData), nil
}

// DecodeFromFile decodes a bencode file to JSON
func DecodeFromFile(inputPath string, opts *DecodeOptions) (string, error) {
	// Open file
	file, err := os.Open(inputPath)
	if err != nil {
		return "", fmt.Errorf("failed to open input file: %w", err)
	}
	defer file.Close()

	// Read all data
	bencodeData, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read input file: %w", err)
	}

	// Decode bencode
	var data interface{}
	if err := bencode.DecodeBytes(bencodeData, &data); err != nil {
		return "", fmt.Errorf("bencode decoding failed: %w", err)
	}

	// Convert to JSON
	var jsonData []byte

	if opts.Pretty {
		jsonData, err = json.MarshalIndent(data, "", "  ")
	} else {
		jsonData, err = json.Marshal(data)
	}

	if err != nil {
		return "", fmt.Errorf("JSON encoding failed: %w", err)
	}

	// Write to file if specified
	if opts.OutputFile != "" {
		if err := os.WriteFile(opts.OutputFile, jsonData, 0644); err != nil {
			return "", fmt.Errorf("failed to write output file: %w", err)
		}
		return "", nil // No string output when writing to file
	}

	return string(jsonData), nil
}

// Run handles bencode commands with routing
func Run(args []string) error {
	// Create router and register all command handlers with their aliases
	router := cli.NewRouter("bt bencode")
	router.RegisterCommand(&cli.Command{Name: "encode", Aliases: []string{"e"}, Description: "Encode JSON to bencode", Run: Encode})
	router.RegisterCommand(&cli.Command{Name: "decode", Aliases: []string{"d"}, Description: "Decode bencode to JSON", Run: Decode})

	// Route to the appropriate command handler
	return router.Route(args)
}

// Encode handles the encode command
func Encode(args []string) error {
	var inputFile string
	var outputFile string

	f := cli.NewCommandFlagSet(
		"encode", []string{"e"},
		"Encode JSON to bencode",
		[]string{"bt bencode encode [options] <json>", "bt bencode encode [options] -f <file>"},
	)
	f.StringVar(&inputFile, "f", "", "input JSON file")
	f.StringVar(&outputFile, "o", "", "output bencode file (writes to stdout if not specified)")

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Handle file input
	if inputFile != "" {
		opts := &EncodeOptions{
			InputFile:  inputFile,
			OutputFile: outputFile,
		}

		result, err := EncodeFromFile(inputFile, opts)
		if err != nil {
			return err
		}

		if result != "" {
			fmt.Print(result)
		} else {
			fmt.Printf("Output written to %s\n", outputFile)
		}
		return nil
	}

	// Handle string input
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing json argument")
	}

	opts := &EncodeOptions{
		OutputFile: outputFile,
	}

	result, err := EncodeFromString(args[0], opts)
	if err != nil {
		return err
	}

	if result != "" {
		fmt.Print(result)
	} else {
		fmt.Printf("Output written to %s\n", outputFile)
	}

	return nil
}

// Decode handles the decode command
func Decode(args []string) error {
	var inputFile string
	var outputFile string
	var pretty bool

	f := cli.NewCommandFlagSet(
		"decode", []string{"d"},
		"Decode bencode to JSON",
		[]string{"bt bencode decode [options] <bencode>", "bt bencode decode [options] -f <file>"},
	)
	f.StringVar(&inputFile, "f", "", "input bencode file")
	f.StringVar(&outputFile, "o", "", "output JSON file (writes to stdout if not specified)")
	f.BoolVar(&pretty, "pretty", false, "pretty-print JSON output")

	err := f.Parse(args)
	if err != nil {
		return err
	}
	args = f.Args()

	// Handle file input
	if inputFile != "" {
		opts := &DecodeOptions{
			InputFile:  inputFile,
			OutputFile: outputFile,
			Pretty:     pretty,
		}

		result, err := DecodeFromFile(inputFile, opts)
		if err != nil {
			return err
		}

		if result != "" {
			fmt.Println(result)
		} else {
			fmt.Printf("Output written to %s\n", outputFile)
		}
		return nil
	}

	// Handle string input
	if len(args) == 0 {
		f.Usage()
		return fmt.Errorf("missing bencode argument")
	}

	opts := &DecodeOptions{
		OutputFile: outputFile,
		Pretty:     pretty || outputFile == "", // Pretty by default for stdout
	}

	result, err := DecodeFromString(args[0], opts)
	if err != nil {
		return err
	}

	if result != "" {
		fmt.Println(result)
	} else {
		fmt.Printf("Output written to %s\n", outputFile)
	}

	return nil
}
