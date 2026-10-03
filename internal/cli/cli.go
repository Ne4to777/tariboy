// Package cli turns registry commands into a docker-style CLI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/client"
	"github.com/alekzonder/tariboy/internal/registry"
)

type Caller interface {
	Call(method, route string, body any) (json.RawMessage, error)
}

func Run(ctx context.Context, reg *registry.Registry, argv []string, call Caller, local *registry.Ctx, out, errOut io.Writer) int {
	jsonOut := false
	rest := make([]string, 0, len(argv))
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) > 0 && rest[0] == "--help-json" {
		b, _ := json.MarshalIndent(reg.Tree(), "", "  ")
		fmt.Fprintln(out, string(b))
		return 0
	}
	if len(rest) == 0 || rest[0] == "--help" || rest[0] == "-h" || rest[0] == "help" {
		printRootHelp(reg, out)
		return 0
	}

	path, args, kind := resolveNode(reg, rest)
	switch kind {
	case "":
		fmt.Fprintf(errOut, "unknown command: %s\n", strings.Join(rest, " "))
		printClosest(reg, rest[0], errOut)
		return 2
	case "group":
		// Bareword `help` mirrors root-level dispatch: `tariboy agent help`
		// prints the group's help, same as `tariboy agent -h`.
		if len(args) == 0 || hasHelpFlag(args) || args[0] == "help" {
			printGroupHelp(reg, path, out)
			return 0
		}
		// Unknown token under a valid group.
		fmt.Fprintf(errOut, "unknown command: %s\n", strings.Join(rest, " "))
		printGroupHelp(reg, path, errOut)
		return 2
	}
	// kind == "command": execute or print detail help.
	cmd, _ := reg.Get(path)
	if hasHelpFlag(args) {
		printCommandHelp(cmd, out)
		return 0
	}

	params, err := parseArgs(cmd, args)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", strings.ReplaceAll(cmd.Path, ".", " "), err)
		printCommandHelp(cmd, errOut)
		return 2
	}
	if cmd.Path == "image.build" || cmd.Path == "image.validate" || cmd.Path == "workflow.build" || cmd.Path == "workflow.validate" {
		if path, ok := params["path"].(string); ok && path != "" {
			absolutePath, absErr := filepath.Abs(path)
			if absErr != nil {
				fmt.Fprintf(errOut, "%s: resolve --path: %v\n", strings.ReplaceAll(cmd.Path, ".", " "), absErr)
				return 1
			}
			params["path"] = absolutePath
		}
	}

	// Follow mode: a command with a follow flag set runs a CLI-local
	// streaming/polling composite over the daemon socket, regardless of whether
	// its non-follow path is remote (logs, channel tail).
	if cmd.Follow != nil && cmd.FollowFlag != "" {
		if f, _ := params[cmd.FollowFlag].(bool); f {
			sock := ""
			if local != nil {
				sock = local.Socket
			}
			if sock == "" {
				fmt.Fprintf(errOut, "%s: follow mode needs a daemon socket\n", strings.ReplaceAll(cmd.Path, ".", " "))
				return 2
			}
			if err := cmd.Follow(ctx, sock, params, out); err != nil {
				return printCLIError(err, errOut)
			}
			return 0
		}
	}
	if cmd.HTTP == nil {
		if local == nil {
			fmt.Fprintf(errOut, "command %s is not available here\n", strings.ReplaceAll(cmd.Path, ".", " "))
			return 2
		}
		result, err := cmd.Handler(local, params)
		if err != nil {
			return printCLIError(err, errOut)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(errOut, "error: %v\n", err)
			return 1
		}
		if jsonOut {
			fmt.Fprintln(out, string(raw))
		} else {
			printHuman(raw, out)
		}
		return resultExitCode(cmd.Path, raw)
	}
	route := cmd.HTTP.Path
	for _, wc := range wildcardNames(cmd.HTTP.Path) {
		v, ok := params[wc]
		if !ok {
			fmt.Fprintf(errOut, "%s: missing %s\n", strings.ReplaceAll(cmd.Path, ".", " "), wc)
			return 2
		}
		route = strings.Replace(route, "{"+wc+"}", url.PathEscape(fmt.Sprintf("%v", v)), 1)
		delete(params, wc)
	}
	var body any = params
	if cmd.HTTP.Method == "GET" || cmd.HTTP.Method == "DELETE" {
		q := map[string]string{}
		for k, v := range params {
			q[k] = fmt.Sprintf("%v", v)
		}
		body = q
	}
	raw, err := call.Call(cmd.HTTP.Method, route, body)
	if err != nil {
		return printCLIError(err, errOut)
	}
	if jsonOut {
		fmt.Fprintln(out, string(raw))
	} else {
		printHuman(raw, out)
	}
	return resultExitCode(cmd.Path, raw)
}

// resultExitCode is 1 for a validate command whose result says the source is
// invalid. The route returns validation errors in its result, not as an HTTP
// error, so without this a script could not gate on the command.
func resultExitCode(path string, raw json.RawMessage) int {
	if !strings.HasSuffix(path, ".validate") {
		return 0
	}
	var result struct {
		Valid *bool `json:"valid"`
	}
	if json.Unmarshal(raw, &result) == nil && result.Valid != nil && !*result.Valid {
		return 1
	}
	return 0
}

// printCLIError renders a handler/transport error to errOut and returns the
// process exit code (2 for "daemon down", 1 otherwise).
func printCLIError(err error, errOut io.Writer) int {
	var ue api.UserError
	if errors.As(err, &ue) {
		fmt.Fprintf(errOut, "error (%s): %s\n", ue.Code, ue.Msg)
		return 1
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		fmt.Fprintf(errOut, "error (%s): %s\n", apiErr.Code, apiErr.Msg)
		return 1
	}
	if client.IsDaemonDown(err) {
		fmt.Fprintln(errOut, "tariboyd is not running (start it with: tariboyd)")
		return 2
	}
	fmt.Fprintf(errOut, "error: %v\n", err)
	return 1
}

// hasHelpFlag reports whether -h/--help appears anywhere in args. These are
// reserved globally and win over any command's short-flag inference.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// resolveNode returns the deepest argv prefix that matches either a command or a
// group. kind is "command", "group", or "" (no match). args is the remaining argv.
func resolveNode(reg *registry.Registry, argv []string) (path string, args []string, kind string) {
	for n := len(argv); n > 0; n-- {
		p := strings.Join(argv[:n], ".")
		if c, ok := reg.Get(p); ok && !c.CLIHidden {
			return p, argv[n:], "command"
		}
		if _, ok := reg.Group(p); ok {
			return p, argv[n:], "group"
		}
	}
	return "", argv, ""
}

func parseArgs(cmd registry.Command, args []string) (registry.Params, error) {
	p := registry.Params{}
	positional := []string{}
	i := 0
	for i < len(args) {
		a := args[i]
		var name string
		isFlag := false
		if strings.HasPrefix(a, "--") {
			name, isFlag = a[2:], true
		} else if len(a) > 1 && a[0] == '-' {
			name, isFlag = a[1:], true
		}
		if !isFlag {
			positional = append(positional, a)
			i++
			continue
		}
		val := ""
		hasVal := false
		if eq := strings.Index(name, "="); eq >= 0 {
			name, val, hasVal = name[:eq], name[eq+1:], true
		}
		arg, ok := findArg(cmd, name)
		if !ok {
			return nil, fmt.Errorf("unknown flag %s", a)
		}
		if arg.Type == registry.Bool {
			if !hasVal {
				// Space-separated form: `--archive true` / `--archive false` both
				// consume the next token as the value; any other next token (a
				// subcommand, a positional, another flag) leaves bare `--archive`
				// meaning true and does not consume it.
				if i+1 < len(args) && (args[i+1] == "true" || args[i+1] == "false") {
					i++
					val = args[i]
				} else {
					val = "true"
				}
			}
		} else if !hasVal {
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("flag %s needs a value", a)
			}
			val = args[i]
		}
		v, err := convert(arg, val)
		if err != nil {
			return nil, err
		}
		if arg.Repeatable {
			values, _ := p[arg.Name].([]string)
			value, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("repeatable argument %s must be a string", arg.Name)
			}
			p[arg.Name] = append(values, value)
		} else {
			p[arg.Name] = v
		}
		i++
	}
	pi := 0
	for _, arg := range cmd.Args {
		if _, set := p[arg.Name]; set {
			continue
		}
		if pi < len(positional) {
			v, err := convert(arg, positional[pi])
			if err != nil {
				return nil, err
			}
			p[arg.Name] = v
			pi++
			continue
		}
		if arg.Required {
			return nil, fmt.Errorf("missing required argument: %s", arg.Name)
		}
		if arg.Default != nil {
			p[arg.Name] = arg.Default
		}
	}
	if pi < len(positional) {
		return nil, fmt.Errorf("unexpected argument: %s", positional[pi])
	}
	return p, nil
}

func findArg(cmd registry.Command, flag string) (registry.Arg, bool) {
	for _, a := range cmd.Args {
		if a.Flag == flag || a.Name == flag {
			return a, true
		}
	}
	for _, a := range cmd.Args {
		if a.Short != "" && a.Short == flag {
			return a, true
		}
	}
	if len(flag) == 1 { // single-dash short flag: match by first letter of Flag/Name
		for _, a := range cmd.Args {
			fn := a.Flag
			if fn == "" {
				fn = a.Name
			}
			if len(fn) > 0 && fn[0] == flag[0] {
				return a, true
			}
		}
	}
	return registry.Arg{}, false
}

func convert(a registry.Arg, s string) (any, error) {
	switch a.Type {
	case registry.JSONObject:
		var value map[string]any
		if err := json.Unmarshal([]byte(s), &value); err != nil || value == nil {
			return nil, fmt.Errorf("argument %s must be a JSON object", a.Name)
		}
		return value, nil
	case registry.Bool:
		switch s {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		default:
			return nil, fmt.Errorf("argument %s: %q is not a boolean (use true/false)", a.Name, s)
		}
	case registry.Int:
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %q is not an integer", a.Name, s)
		}
		return n, nil
	case registry.IntegerList:
		if strings.TrimSpace(s) == "" {
			return []int64{}, nil
		}
		parts := strings.Split(s, ",")
		values := make([]int64, 0, len(parts))
		for _, part := range parts {
			n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("argument %s: %q is not a comma-separated integer list", a.Name, s)
			}
			values = append(values, n)
		}
		return values, nil
	default:
		return s, nil
	}
}

func printHuman(raw json.RawMessage, out io.Writer) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch m[k].(type) {
			case map[string]any, []any:
				b, _ := json.MarshalIndent(m[k], "", "  ")
				fmt.Fprintf(out, "%s:\n%s\n", k, indentBlock(string(b)))
			default:
				fmt.Fprintf(out, "%s: %v\n", k, m[k])
			}
		}
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		fmt.Fprintln(out, s)
		return
	}
	fmt.Fprintln(out, strings.Trim(string(raw), "\"\n"))
}

// indentBlock prefixes each line of s with two spaces for nested-value display.
func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// childrenOf returns the direct group and command children of a group prefix
// ("" for the root). Names are the last path segment.
func childrenOf(reg *registry.Registry, prefix string) (groups, cmds [][2]string) {
	seen := map[string]bool{}
	depth := 0
	if prefix != "" {
		depth = len(strings.Split(prefix, "."))
	}
	add := func(store *[][2]string, full, summary string) {
		parts := strings.Split(full, ".")
		if len(parts) != depth+1 {
			return
		}
		if prefix != "" && !strings.HasPrefix(full, prefix+".") {
			return
		}
		name := parts[len(parts)-1]
		if seen[name] {
			return
		}
		seen[name] = true
		*store = append(*store, [2]string{name, summary})
	}
	for _, g := range reg.Groups() {
		add(&groups, g.Path, g.Summary)
	}
	for _, c := range reg.Commands() {
		if c.CLIHidden {
			continue
		}
		add(&cmds, c.Path, c.Summary)
	}
	return groups, cmds
}

func printSections(out io.Writer, groups, cmds [][2]string) {
	if len(groups) > 0 {
		fmt.Fprintln(out, "Command groups:")
		for _, g := range groups {
			fmt.Fprintf(out, "  %-16s %s\n", g[0], g[1])
		}
	}
	if len(cmds) > 0 {
		if len(groups) > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out, "Commands:")
		for _, c := range cmds {
			fmt.Fprintf(out, "  %-16s %s\n", c[0], c[1])
		}
	}
}

func printRootHelp(reg *registry.Registry, out io.Writer) {
	fmt.Fprintln(out, "Usage: tariboy <command> [args]")
	fmt.Fprintln(out)
	groups, cmds := childrenOf(reg, "")
	printSections(out, groups, cmds)
	fmt.Fprintln(out, "\nGlobal flags: --json, --help, -h, --help-json, --version")
}

func printGroupHelp(reg *registry.Registry, path string, out io.Writer) {
	display := strings.ReplaceAll(path, ".", " ")
	fmt.Fprintf(out, "Usage: tariboy %s <command> [args]\n", display)
	if s, ok := reg.Group(path); ok {
		fmt.Fprintf(out, "\n%s\n", s)
	}
	fmt.Fprintln(out)
	groups, cmds := childrenOf(reg, path)
	printSections(out, groups, cmds)
}

func printCommandHelp(cmd registry.Command, out io.Writer) {
	fmt.Fprintf(out, "Usage: tariboy %s", strings.ReplaceAll(cmd.Path, ".", " "))
	for _, a := range cmd.Args {
		if a.Required {
			fmt.Fprintf(out, " <%s>", a.Name)
		} else {
			fmt.Fprintf(out, " [--%s %s]", flagName(a), a.Type)
		}
	}
	fmt.Fprintf(out, "\n\n%s\n", cmd.Summary)
	if cmd.Help != "" {
		fmt.Fprintf(out, "\n%s\n", cmd.Help)
	}
	if len(cmd.Args) > 0 {
		fmt.Fprintln(out, "\nArguments:")
		for _, a := range cmd.Args {
			req := ""
			if a.Required {
				req = " (required)"
			}
			fmt.Fprintf(out, "  --%-16s %s%s\n", flagName(a), a.Help, req)
		}
	}
}

func flagName(a registry.Arg) string {
	if a.Flag != "" {
		return a.Flag
	}
	return a.Name
}

func wildcardNames(pattern string) []string {
	var out []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

func printClosest(reg *registry.Registry, word string, out io.Writer) {
	for _, c := range reg.Commands() {
		if c.CLIHidden {
			continue
		}
		if strings.HasPrefix(c.Path, word) {
			fmt.Fprintf(out, "  did you mean: %s?\n", strings.ReplaceAll(c.Path, ".", " "))
		}
	}
}
