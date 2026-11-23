package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/galaxy-io/tempo/internal/temporal"
)

// Context holds the variables available for template expansion.
type Context struct {
	WorkflowID   string
	RunID        string
	WorkflowType string
	Namespace    string
	Address      string
	Profile      string
	Args         []string

	// Connection details for injecting into temporal CLI commands
	TLSCertPath   string
	TLSKeyPath    string
	TLSCAPath     string
	TLSServerName string
	TLSSkipVerify bool
	APIKey        string
}

// Invocation is a command template prepared for execution.
type Invocation struct {
	// Script is the shell source to run. It refers to every substituted value
	// through an environment variable, so no value is parsed as shell syntax.
	Script string
	// Env holds the KEY=VALUE entries Script and the Temporal CLI read.
	Env []string
	// Display is the command with values substituted in place, for showing to
	// the user. It carries no credentials and is not meant to be executed.
	Display string
}

// setEnv records an environment variable for the invocation, replacing any
// earlier value recorded under the same name.
func (inv *Invocation) setEnv(name, value string) {
	entry := name + "=" + value
	for i, existing := range inv.Env {
		if strings.HasPrefix(existing, name+"=") {
			inv.Env[i] = entry
			return
		}
	}
	inv.Env = append(inv.Env, entry)
}

var placeholderRe = regexp.MustCompile(`^\{([^}]+)\}`)

// quoteState is the shell quoting in effect at a position in a template.
type quoteState int

const (
	quoteNone quoteState = iota
	quoteSingle
	quoteDouble
)

// envReference returns shell source that expands the named environment
// variable to exactly one word in the given quoting context.
func envReference(name string, quote quoteState) string {
	switch quote {
	case quoteDouble:
		return "${" + name + "}"
	case quoteSingle:
		return `'"${` + name + `}"'`
	default:
		return `"${` + name + `}"`
	}
}

// resolveVariable returns the environment variable name and value for a
// template placeholder key. It fails for an unknown key, a positional argument
// that was not supplied, or a variable with an empty value.
func resolveVariable(key string, ctx Context) (name, value string, err error) {
	switch key {
	case "workflow_id":
		name, value = "TEMPO_WORKFLOW_ID", ctx.WorkflowID
	case "run_id":
		name, value = "TEMPO_RUN_ID", ctx.RunID
	case "workflow_type":
		name, value = "TEMPO_WORKFLOW_TYPE", ctx.WorkflowType
	case "namespace":
		name, value = "TEMPO_NAMESPACE", ctx.Namespace
	case "address":
		name, value = "TEMPO_ADDRESS", ctx.Address
	case "profile":
		name, value = "TEMPO_PROFILE", ctx.Profile
	default:
		// Positional args: {1}, {2}, etc.
		n, convErr := strconv.Atoi(key)
		if convErr != nil || n < 1 {
			return "", "", fmt.Errorf("unknown variable {%s}", key)
		}
		if n > len(ctx.Args) {
			return "", "", fmt.Errorf("positional argument {%s} not provided", key)
		}
		name, value = "TEMPO_ARG_"+strconv.Itoa(n), ctx.Args[n-1]
	}
	if value == "" {
		return "", "", fmt.Errorf("variable {%s} is empty", key)
	}
	return name, value, nil
}

// ExpandCmd prepares a command template for execution, resolving its {var}
// placeholders from ctx. Each value reaches the command as a single word,
// whatever characters it contains: the script reads it from an environment
// variable (TEMPO_WORKFLOW_ID, TEMPO_ARG_1, ...) instead of embedding it. A
// backslash before "{" keeps the braces literal.
// Returns an error if a placeholder is unknown or its value is empty.
func ExpandCmd(template string, ctx Context) (Invocation, error) {
	var inv Invocation
	var script, display strings.Builder
	quote := quoteNone

	for i := 0; i < len(template); {
		c := template[i]

		if c == '{' {
			if match := placeholderRe.FindStringSubmatch(template[i:]); match != nil {
				name, value, err := resolveVariable(match[1], ctx)
				if err != nil {
					return Invocation{}, err
				}
				inv.setEnv(name, value)
				script.WriteString(envReference(name, quote))
				display.WriteString(value)
				i += len(match[0])
				continue
			}
		}

		// A backslash outside single quotes escapes the next character, so
		// that character neither opens nor closes a quote.
		if c == '\\' && quote != quoteSingle && i+1 < len(template) {
			script.WriteString(template[i : i+2])
			display.WriteString(template[i : i+2])
			i += 2
			continue
		}

		switch {
		case c == '\'' && quote == quoteNone:
			quote = quoteSingle
		case c == '\'' && quote == quoteSingle:
			quote = quoteNone
		case c == '"' && quote == quoteNone:
			quote = quoteDouble
		case c == '"' && quote == quoteDouble:
			quote = quoteNone
		}

		script.WriteByte(c)
		display.WriteByte(c)
		i++
	}

	inv.Script = script.String()
	inv.Display = display.String()
	return inv, nil
}

// InjectConnectionFlags makes a temporal CLI command connect to the active
// profile's server by appending --address and --tls-* flags that the command
// does not already carry. The API key is passed in the TEMPORAL_API_KEY
// environment variable, so it appears in neither the command line nor Display.
// Only modifies commands that start with "temporal ".
func InjectConnectionFlags(inv Invocation, ctx Context) Invocation {
	cmdStr := inv.Display
	if !strings.HasPrefix(cmdStr, "temporal ") {
		return inv
	}

	inv.Env = append([]string(nil), inv.Env...)

	var scriptFlags, displayFlags []string
	addValueFlag := func(flag, envName, value string) {
		inv.setEnv(envName, value)
		scriptFlags = append(scriptFlags, flag+" "+envReference(envName, quoteNone))
		displayFlags = append(displayFlags, fmt.Sprintf("%s %q", flag, value))
	}
	addFlag := func(flag string) {
		scriptFlags = append(scriptFlags, flag)
		displayFlags = append(displayFlags, flag)
	}

	// Only add flags that aren't already present in the command
	if ctx.Address != "" && !strings.Contains(cmdStr, "--address") {
		addValueFlag("--address", "TEMPO_ADDRESS", ctx.Address)
	}
	if ctx.TLSCertPath != "" && !strings.Contains(cmdStr, "--tls-cert") {
		addValueFlag("--tls-cert-path", "TEMPO_TLS_CERT_PATH", ctx.TLSCertPath)
	}
	if ctx.TLSKeyPath != "" && !strings.Contains(cmdStr, "--tls-key") {
		addValueFlag("--tls-key-path", "TEMPO_TLS_KEY_PATH", ctx.TLSKeyPath)
	}
	if ctx.TLSCAPath != "" && !strings.Contains(cmdStr, "--tls-ca") {
		addValueFlag("--tls-ca-path", "TEMPO_TLS_CA_PATH", ctx.TLSCAPath)
	}
	if ctx.TLSServerName != "" && !strings.Contains(cmdStr, "--tls-server-name") {
		addValueFlag("--tls-server-name", "TEMPO_TLS_SERVER_NAME", ctx.TLSServerName)
	}
	if ctx.TLSSkipVerify && !strings.Contains(cmdStr, "--tls-disable-host-verification") {
		addFlag("--tls-disable-host-verification")
	}

	// Enable base TLS if any TLS option is set.
	// Check for "--tls " or "--tls" at end of string to avoid matching --tls-* flags.
	needsTLS := ctx.TLSCertPath != "" || ctx.TLSKeyPath != "" || ctx.TLSCAPath != "" ||
		ctx.TLSServerName != "" || ctx.TLSSkipVerify || ctx.APIKey != ""
	hasTLSFlag := strings.Contains(cmdStr, "--tls ") || strings.HasSuffix(cmdStr, "--tls")
	if needsTLS && !hasTLSFlag {
		addFlag("--tls")
	}
	if ctx.APIKey != "" && !strings.Contains(cmdStr, "--api-key") {
		inv.setEnv("TEMPORAL_API_KEY", ctx.APIKey)
	}

	if len(scriptFlags) > 0 {
		inv.Script += " " + strings.Join(scriptFlags, " ")
		inv.Display += " " + strings.Join(displayFlags, " ")
	}
	return inv
}

// shellCommand builds the sh -c process for an invocation, adding its
// variables to the current environment.
func shellCommand(ctx context.Context, inv Invocation) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", inv.Script)
	cmd.Env = append(os.Environ(), inv.Env...)
	return cmd
}

// Run executes an invocation via sh -c and returns combined output.
func Run(ctx context.Context, inv Invocation) (string, error) {
	out, err := shellCommand(ctx, inv).CombinedOutput()
	return string(out), err
}

// RunStreaming executes an invocation with line-by-line streaming via scanner on stdout pipe.
// Stderr is merged into stdout.
func RunStreaming(ctx context.Context, inv Invocation, onLine func(string)) error {
	cmd := shellCommand(ctx, inv)
	cmd.Stderr = nil // will be merged via 2>&1 in the shell command or we merge pipes

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting command: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		onLine(scanner.Text())
	}

	return cmd.Wait()
}

// ParseWorkflowsOutput parses JSONL output from temporal workflow list --output json.
// Supports both temporal CLI v1 (flat keys) and v2 (nested execution/type objects).
func ParseWorkflowsOutput(output string) ([]temporal.Workflow, error) {
	// First, try parsing as a JSON array (some CLI versions output an array)
	output = strings.TrimSpace(output)
	if strings.HasPrefix(output, "[") {
		var arr []map[string]interface{}
		if err := json.Unmarshal([]byte(output), &arr); err == nil {
			var workflows []temporal.Workflow
			for _, raw := range arr {
				if wf := parseWorkflowJSON(raw); wf.ID != "" {
					workflows = append(workflows, wf)
				}
			}
			if len(workflows) > 0 {
				return workflows, nil
			}
		}
	}

	// Otherwise, parse as JSONL (one JSON object per line)
	var workflows []temporal.Workflow
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		if wf := parseWorkflowJSON(raw); wf.ID != "" {
			workflows = append(workflows, wf)
		}
	}
	if len(workflows) == 0 {
		return nil, fmt.Errorf("no workflows found in output")
	}
	return workflows, nil
}

// parseWorkflowJSON extracts a Workflow from a raw JSON map.
// Handles multiple key formats from different temporal CLI versions.
func parseWorkflowJSON(raw map[string]interface{}) temporal.Workflow {
	return temporal.Workflow{
		ID:        jsonStr(raw, "workflowId", "execution.workflowId", "workflowExecutionInfo.execution.workflowId"),
		RunID:     jsonStr(raw, "runId", "execution.runId", "workflowExecutionInfo.execution.runId"),
		Type:      jsonStr(raw, "workflowType", "type.name", "workflowExecutionInfo.type.name"),
		Status:    normalizeStatus(jsonStr(raw, "status", "workflowExecutionInfo.status")),
		TaskQueue: jsonStr(raw, "taskQueue", "workflowExecutionInfo.taskQueue"),
	}
}

// ParseWorkflowOutput parses JSON output from temporal workflow describe --output json.
func ParseWorkflowOutput(output string) (workflowID, runID string, err error) {
	output = strings.TrimSpace(output)
	// Find the first { in output (skip any non-JSON preamble from CLI)
	if idx := strings.Index(output, "{"); idx > 0 {
		output = output[idx:]
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(output), &raw); err != nil {
		return "", "", fmt.Errorf("parsing workflow JSON: %w", err)
	}

	workflowID = jsonStr(raw, "workflowId", "execution.workflowId", "workflowExecutionInfo.execution.workflowId")
	runID = jsonStr(raw, "runId", "execution.runId", "workflowExecutionInfo.execution.runId")

	if workflowID == "" {
		return "", "", fmt.Errorf("no workflowId found in output")
	}
	return workflowID, runID, nil
}

// jsonStr extracts a string value from a map, trying multiple key paths.
func jsonStr(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		parts := strings.Split(key, ".")
		current := m
		for i, part := range parts {
			if i == len(parts)-1 {
				if v, ok := current[part]; ok {
					if s, ok := v.(string); ok {
						return s
					}
				}
			} else {
				if nested, ok := current[part].(map[string]interface{}); ok {
					current = nested
				} else {
					break
				}
			}
		}
	}
	return ""
}

// normalizeStatus converts Temporal CLI status format to display format.
func normalizeStatus(s string) string {
	s = strings.TrimPrefix(s, "WORKFLOW_EXECUTION_STATUS_")
	switch strings.ToUpper(s) {
	case "RUNNING":
		return "Running"
	case "COMPLETED":
		return "Completed"
	case "FAILED":
		return "Failed"
	case "CANCELED":
		return "Canceled"
	case "TERMINATED":
		return "Terminated"
	case "TIMED_OUT":
		return "TimedOut"
	default:
		if s != "" {
			// Title case the first letter
			return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
		}
		return s
	}
}
