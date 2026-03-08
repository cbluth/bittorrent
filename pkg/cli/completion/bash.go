package completion

import (
	"fmt"
	"strings"
)

// BashCompletion generates a data-driven bash completion script.
// The script can be sourced to enable tab completion: source <(cmd completion bash)
func BashCompletion(appName string, commands []CommandInfo, globalFlags []FlagInfo) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, `# bash completion for %s | -*- shell-script -*-

function _%s_completion {
    local cur prev words cword
    _init_completion || return

`, appName, appName)

	// Build command list (names + aliases).
	var allCommands []string
	for _, cmd := range commands {
		allCommands = append(allCommands, cmd.Name)
		allCommands = append(allCommands, cmd.Aliases...)
	}
	fmt.Fprintf(&sb, "    local all_commands=\"%s\"\n", strings.Join(allCommands, " "))

	// Global flags.
	var globalFlagNames []string
	for _, f := range globalFlags {
		globalFlagNames = append(globalFlagNames, fmt.Sprintf("-%s", f.Name))
	}
	fmt.Fprintf(&sb, "    local global_flags=\"%s\"\n\n", strings.Join(globalFlagNames, " "))

	// Find the active command (first non-flag argument).
	fmt.Fprintf(&sb, `    # Get the command being completed (first non-flag argument)
    local command=""
    local i
    for ((i=1; i < cword; i++)); do
        if [[ "${words[i]}" != -* ]]; then
            command="${words[i]}"
            break
        fi
    done

    # If no command yet, complete with global flags and commands
    if [[ -z "$command" || "$cword" -eq 1 ]]; then
        COMPREPLY=($(compgen -W "$global_flags $all_commands" -- "$cur"))
        return 0
    fi

    # Per-command completion
    case "$command" in
`)

	// Emit a case branch for each command.
	for _, cmd := range commands {
		writeCommandCase(&sb, cmd)
	}

	// Built-in completion command.
	fmt.Fprintf(&sb, `        completion)
            if [[ "$prev" == "completion" ]]; then
                COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
            fi
            ;;
`)

	// Default / unknown command.
	fmt.Fprintf(&sb, `        *)
            _filedir
            ;;
    esac
}

`)

	fmt.Fprintf(&sb, "complete -F _%s_completion %s\n", appName, appName)
	sb.WriteString("# vim: ft=sh\n")

	return sb.String()
}

// writeCommandCase writes a single case branch for a command.
// When cmd.SubCommands is set a nested subcommand case is emitted instead of
// positional-arg completion.
func writeCommandCase(sb *strings.Builder, cmd CommandInfo) {
	// Pattern: name|alias1|alias2
	allNames := make([]string, 0, 1+len(cmd.Aliases))
	allNames = append(allNames, cmd.Name)
	allNames = append(allNames, cmd.Aliases...)
	pattern := strings.Join(allNames, "|")

	fmt.Fprintf(sb, "        %s)\n", pattern)

	if len(cmd.SubCommands) > 0 {
		writeSubCommandCase(sb, cmd.SubCommands)
		sb.WriteString("            ;;\n")
		return
	}

	// Collect flag names for this command.
	var flagNames []string
	flagValues := map[string][]string{} // flag name -> possible values
	for _, f := range cmd.Flags {
		flagNames = append(flagNames, fmt.Sprintf("-%s", f.Name))
		if len(f.Values) > 0 {
			flagValues[f.Name] = f.Values
		}
	}

	// If $cur starts with -, complete flags.
	if len(flagNames) > 0 {
		fmt.Fprintf(sb, "            if [[ \"$cur\" == -* ]]; then\n")
		fmt.Fprintf(sb, "                COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(flagNames, " "))
		fmt.Fprintf(sb, "                return 0\n")
		fmt.Fprintf(sb, "            fi\n")
	}

	// If $prev is a flag that has known values, complete those.
	if len(flagValues) > 0 {
		sb.WriteString("            case \"$prev\" in\n")
		for name, vals := range flagValues {
			fmt.Fprintf(sb, "                -%s)\n", name)
			fmt.Fprintf(sb, "                    COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(vals, " "))
			sb.WriteString("                    return 0\n")
			sb.WriteString("                    ;;\n")
		}
		sb.WriteString("            esac\n")
	}

	// Positional argument completion.
	switch {
	case cmd.NoArgCompletion:
		sb.WriteString("            return 0\n")
	case len(cmd.CompletionWords) > 0:
		fmt.Fprintf(sb, "            COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(cmd.CompletionWords, " "))
	case cmd.CompletionFile != "":
		fmt.Fprintf(sb, "            _filedir '%s'\n", cmd.CompletionFile)
	default:
		sb.WriteString("            _filedir\n")
	}

	sb.WriteString("            ;;\n")
}

// writeSubCommandCase emits the nested subcommand logic for a parent command.
// It uses the $i variable (set by the outer command-finding loop) to scan for
// the subcommand starting at position i+1 in $words.
func writeSubCommandCase(sb *strings.Builder, subs []CommandInfo) {
	// Collect all subcommand names (canonical names only) for the word list.
	var subNames []string
	for _, s := range subs {
		subNames = append(subNames, s.Name)
	}

	sb.WriteString("            local subcmd=\"\"\n")
	sb.WriteString("            local j\n")
	sb.WriteString("            for ((j=i+1; j < cword; j++)); do\n")
	sb.WriteString("                if [[ \"${words[j]}\" != -* ]]; then\n")
	sb.WriteString("                    subcmd=\"${words[j]}\"\n")
	sb.WriteString("                    break\n")
	sb.WriteString("                fi\n")
	sb.WriteString("            done\n")
	fmt.Fprintf(sb, "            if [[ -z \"$subcmd\" ]]; then\n")
	fmt.Fprintf(sb, "                COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(subNames, " "))
	sb.WriteString("                return 0\n")
	sb.WriteString("            fi\n")
	sb.WriteString("            case \"$subcmd\" in\n")

	for _, sub := range subs {
		// Pattern includes aliases.
		allNames := make([]string, 0, 1+len(sub.Aliases))
		allNames = append(allNames, sub.Name)
		allNames = append(allNames, sub.Aliases...)
		pattern := strings.Join(allNames, "|")
		fmt.Fprintf(sb, "                %s)\n", pattern)

		// Per-subcommand flag completion.
		var flagNames []string
		flagValues := map[string][]string{}
		for _, f := range sub.Flags {
			flagNames = append(flagNames, fmt.Sprintf("-%s", f.Name))
			if len(f.Values) > 0 {
				flagValues[f.Name] = f.Values
			}
		}
		if len(flagNames) > 0 {
			fmt.Fprintf(sb, "                    if [[ \"$cur\" == -* ]]; then\n")
			fmt.Fprintf(sb, "                        COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(flagNames, " "))
			fmt.Fprintf(sb, "                        return 0\n")
			fmt.Fprintf(sb, "                    fi\n")
		}
		if len(flagValues) > 0 {
			sb.WriteString("                    case \"$prev\" in\n")
			for name, vals := range flagValues {
				fmt.Fprintf(sb, "                        -%s)\n", name)
				fmt.Fprintf(sb, "                            COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(vals, " "))
				sb.WriteString("                            return 0\n")
				sb.WriteString("                            ;;\n")
			}
			sb.WriteString("                    esac\n")
		}

		// Positional arg for the subcommand.
		switch {
		case sub.NoArgCompletion:
			sb.WriteString("                    return 0\n")
		case len(sub.CompletionWords) > 0:
			fmt.Fprintf(sb, "                    COMPREPLY=($(compgen -W \"%s\" -- \"$cur\"))\n", strings.Join(sub.CompletionWords, " "))
		case sub.CompletionFile != "":
			fmt.Fprintf(sb, "                    _filedir '%s'\n", sub.CompletionFile)
		default:
			sb.WriteString("                    _filedir\n")
		}
		sb.WriteString("                    ;;\n")
	}

	sb.WriteString("                *)\n")
	sb.WriteString("                    _filedir\n")
	sb.WriteString("                    ;;\n")
	sb.WriteString("            esac\n")
}
