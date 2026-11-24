package command

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExpandCmdReferencesValuesThroughEnvironment(t *testing.T) {
	ctx := Context{
		WorkflowID: "order-42",
		Namespace:  "payments",
		Args:       []string{"first arg"},
	}

	tests := []struct {
		name     string
		template string
		script   string
		display  string
		env      []string
	}{
		{
			name:     "unquoted placeholder",
			template: `temporal workflow show -w {workflow_id}`,
			script:   `temporal workflow show -w "${TEMPO_WORKFLOW_ID}"`,
			display:  `temporal workflow show -w order-42`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
		{
			name:     "double-quoted placeholder",
			template: `echo "id: {workflow_id} in {namespace}"`,
			script:   `echo "id: ${TEMPO_WORKFLOW_ID} in ${TEMPO_NAMESPACE}"`,
			display:  `echo "id: order-42 in payments"`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_NAMESPACE=payments"},
		},
		{
			name:     "single-quoted placeholder",
			template: `echo 'id: {workflow_id}'`,
			script:   `echo 'id: '"${TEMPO_WORKFLOW_ID}"''`,
			display:  `echo 'id: order-42'`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
		{
			name:     "apostrophe inside double quotes does not open a quote",
			template: `echo "it's {workflow_id}" {1}`,
			script:   `echo "it's ${TEMPO_WORKFLOW_ID}" "${TEMPO_ARG_1}"`,
			display:  `echo "it's order-42" first arg`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_ARG_1=first arg"},
		},
		{
			name:     "escaped quote does not open a quote",
			template: `echo \" {workflow_id}`,
			script:   `echo \" "${TEMPO_WORKFLOW_ID}"`,
			display:  `echo \" order-42`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
		{
			name:     "escaped brace stays literal",
			template: `echo \{workflow_id} {workflow_id}`,
			script:   `echo \{workflow_id} "${TEMPO_WORKFLOW_ID}"`,
			display:  `echo {workflow_id} order-42`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
		{
			name:     "escaped brace inside single quotes",
			template: `printf '%s\n' '\{not_a_variable}' '{workflow_id}'`,
			script:   `printf '%s\n' '{not_a_variable}' ''"${TEMPO_WORKFLOW_ID}"''`,
			display:  `printf '%s\n' '{not_a_variable}' 'order-42'`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
		{
			name:     "escaped brace inside double quotes",
			template: `echo "\{not_a_variable}"`,
			script:   `echo "{not_a_variable}"`,
			display:  `echo "{not_a_variable}"`,
		},
		{
			name:     "command substitution has its own quoting",
			template: `printf '%s' "$(printf '%s' '{workflow_id}' {namespace})"`,
			script:   `printf '%s' "$(printf '%s' ''"${TEMPO_WORKFLOW_ID}"'' "${TEMPO_NAMESPACE}")"`,
			display:  `printf '%s' "$(printf '%s' 'order-42' payments)"`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_NAMESPACE=payments"},
		},
		{
			name:     "outer quoting resumes after a command substitution",
			template: `echo "$(date "+%s") {workflow_id}" {namespace}`,
			script:   `echo "$(date "+%s") ${TEMPO_WORKFLOW_ID}" "${TEMPO_NAMESPACE}"`,
			display:  `echo "$(date "+%s") order-42" payments`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_NAMESPACE=payments"},
		},
		{
			name:     "parentheses inside a command substitution",
			template: `echo "$( (echo {workflow_id}) ) {namespace}"`,
			script:   `echo "$( (echo "${TEMPO_WORKFLOW_ID}") ) ${TEMPO_NAMESPACE}"`,
			display:  `echo "$( (echo order-42) ) payments"`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_NAMESPACE=payments"},
		},
		{
			name:     "backticks have their own quoting",
			template: "echo \"`printf '%s' {workflow_id}` {namespace}\"",
			script:   "echo \"`printf '%s' \"${TEMPO_WORKFLOW_ID}\"` ${TEMPO_NAMESPACE}\"",
			display:  "echo \"`printf '%s' order-42` payments\"",
			env:      []string{"TEMPO_WORKFLOW_ID=order-42", "TEMPO_NAMESPACE=payments"},
		},
		{
			name:     "repeated placeholder is recorded once",
			template: `echo {workflow_id} {workflow_id}`,
			script:   `echo "${TEMPO_WORKFLOW_ID}" "${TEMPO_WORKFLOW_ID}"`,
			display:  `echo order-42 order-42`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := ExpandCmd(tt.template, ctx)
			if err != nil {
				t.Fatalf("ExpandCmd: %v", err)
			}
			if inv.Script != tt.script {
				t.Errorf("script = %q, want %q", inv.Script, tt.script)
			}
			if inv.Display != tt.display {
				t.Errorf("display = %q, want %q", inv.Display, tt.display)
			}
			if !reflect.DeepEqual(inv.Env, tt.env) {
				t.Errorf("env = %q, want %q", inv.Env, tt.env)
			}
		})
	}
}

func TestExpandCmdRejectsUnresolvedPlaceholders(t *testing.T) {
	tests := []struct {
		name     string
		template string
		ctx      Context
		want     string
	}{
		{"unknown variable", `echo {nope}`, Context{}, "unknown variable {nope}"},
		{"empty variable", `echo {workflow_id}`, Context{}, "variable {workflow_id} is empty"},
		{"missing positional argument", `echo {2}`, Context{Args: []string{"one"}}, "positional argument {2} not provided"},
		{"zero positional argument", `echo {0}`, Context{Args: []string{"one"}}, "unknown variable {0}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExpandCmd(tt.template, tt.ctx)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

// A value chosen by whoever started the workflow must reach the command as
// data, in every quoting context the template can put it in.
func TestRunNeverExecutesSubstitutedValues(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	templates := []struct {
		name     string
		template string
		// exact reports whether the command must print the value unchanged.
		exact bool
	}{
		{"unquoted", `printf '%s' {workflow_id}`, true},
		{"double-quoted", `printf '%s' "{workflow_id}"`, true},
		{"single-quoted", `printf '%s' '{workflow_id}'`, true},
		{"positional argument", `printf '%s' {1}`, true},
		{"inside command substitution", `printf '%s' "$(printf '%s' {workflow_id})"`, true},
		{"quoted inside command substitution", `printf '%s' "$(printf '%s' '{workflow_id}')"`, true},
		{"inside backticks", "echo `printf '%s' {workflow_id}`", false},
	}

	for _, tt := range templates {
		for valueName, hostile := range hostilePayloads(marker) {
			t.Run(tt.name+"/"+valueName, func(t *testing.T) {
				inv, err := ExpandCmd(tt.template, Context{WorkflowID: hostile, Args: []string{hostile}})
				if err != nil {
					t.Fatalf("ExpandCmd: %v", err)
				}
				if strings.Contains(inv.Script, "touch") {
					t.Fatalf("script embeds the value: %q", inv.Script)
				}

				out, err := Run(context.Background(), inv)
				if err != nil {
					t.Fatalf("Run: %v\n%s", err, out)
				}
				if _, statErr := os.Stat(marker); statErr == nil {
					t.Fatalf("value was executed as shell code; output: %q", out)
				}
				if tt.exact && out != hostile {
					t.Errorf("output = %q, want %q", out, hostile)
				}
			})
		}
	}
}

// The shell evaluates a value used in arithmetic, and bash runs command
// substitutions found in an array subscript there, so only integers may pass.
func TestExpandCmdAllowsOnlyIntegersInArithmetic(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")

	templates := []struct {
		name     string
		template string
		// run reports whether the template is portable enough to execute.
		run  bool
		want string
	}{
		{"unquoted", `echo $(( {1} + 1 ))`, true, "3\n"},
		{"double-quoted", `echo "$(( {1} + 1 ))"`, true, "3\n"},
		{"grouped", `echo $(( ({1} + 1) * 2 ))`, true, "6\n"},
		{"substitution inside arithmetic", `echo $(( $(printf '%s' {1}) + 1 ))`, true, "3\n"},
		{"arithmetic command", `(( {1} > 1 )) && echo yes`, false, ""},
	}
	hostileValues := []string{
		"probe[$(touch " + marker + ")0]",
		"$(touch " + marker + ")",
		"2; touch " + marker,
		"1 + 1",
		"two",
	}

	for _, tt := range templates {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := ExpandCmd(tt.template, Context{Args: []string{"2"}})
			if err != nil {
				t.Fatalf("ExpandCmd with an integer: %v", err)
			}
			if tt.run {
				out, err := Run(context.Background(), inv)
				if err != nil {
					t.Fatalf("Run: %v\n%s", err, out)
				}
				if out != tt.want {
					t.Errorf("output = %q, want %q", out, tt.want)
				}
			}

			for _, hostile := range hostileValues {
				_, err := ExpandCmd(tt.template, Context{Args: []string{hostile}})
				want := "variable {1} must be an integer inside an arithmetic expression"
				if err == nil || err.Error() != want {
					t.Errorf("value %q: error = %v, want %q", hostile, err, want)
				}
			}
		})
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a value was executed as shell code")
	}
}

func TestExpandCmdLeavesArithmeticRuleAtTheClosingParentheses(t *testing.T) {
	inv, err := ExpandCmd(`echo $(( {1} + 1 )) {workflow_id}`, Context{WorkflowID: "not a number", Args: []string{"-4"}})
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}
	if want := `echo $(( ${TEMPO_ARG_1} + 1 )) "${TEMPO_WORKFLOW_ID}"`; inv.Script != want {
		t.Errorf("script = %q, want %q", inv.Script, want)
	}
}

func TestRunStreamingPassesEnvironment(t *testing.T) {
	inv, err := ExpandCmd(`printf '%s\n' {workflow_id} {run_id}`, Context{WorkflowID: "a b", RunID: "c;d"})
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}

	var lines []string
	if err := RunStreaming(context.Background(), inv, func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}
	if want := []string{"a b", "c;d"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}
}

func TestInjectConnectionFlagsKeepsAPIKeyOutOfCommandLine(t *testing.T) {
	ctx := Context{
		WorkflowID:  "order-42",
		Address:     "ns.tmprl.cloud:7233",
		TLSCertPath: "/certs/$(whoami).pem",
		APIKey:      "secret-key",
	}
	inv, err := ExpandCmd(`temporal workflow show -w {workflow_id}`, ctx)
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}

	inv = InjectConnectionFlags(inv, ctx)

	wantScript := `temporal workflow show -w "${TEMPO_WORKFLOW_ID}"` +
		` --address "${TEMPO_ADDRESS}" --tls-cert-path "${TEMPO_TLS_CERT_PATH}" --tls`
	if inv.Script != wantScript {
		t.Errorf("script = %q, want %q", inv.Script, wantScript)
	}
	wantDisplay := `temporal workflow show -w order-42` +
		` --address "ns.tmprl.cloud:7233" --tls-cert-path "/certs/$(whoami).pem" --tls`
	if inv.Display != wantDisplay {
		t.Errorf("display = %q, want %q", inv.Display, wantDisplay)
	}
	wantEnv := []string{
		"TEMPO_WORKFLOW_ID=order-42",
		"TEMPO_ADDRESS=ns.tmprl.cloud:7233",
		"TEMPO_TLS_CERT_PATH=/certs/$(whoami).pem",
		"TEMPORAL_API_KEY=secret-key",
	}
	if !reflect.DeepEqual(inv.Env, wantEnv) {
		t.Errorf("env = %q, want %q", inv.Env, wantEnv)
	}
	if strings.Contains(inv.Script, "secret-key") || strings.Contains(inv.Display, "secret-key") {
		t.Errorf("API key leaked into the command: script %q, display %q", inv.Script, inv.Display)
	}
}

func TestInjectConnectionFlagsRespectsExistingFlags(t *testing.T) {
	ctx := Context{Address: "ns.tmprl.cloud:7233", APIKey: "secret-key"}
	inv, err := ExpandCmd(`temporal workflow list --address other:7233 --tls --api-key "$MY_KEY"`, ctx)
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}
	before := inv

	inv = InjectConnectionFlags(inv, ctx)

	if inv.Script != before.Script || inv.Display != before.Display {
		t.Errorf("command changed: script %q, display %q", inv.Script, inv.Display)
	}
	if len(inv.Env) != 0 {
		t.Errorf("env = %q, want none", inv.Env)
	}
}

func TestInjectConnectionFlagsLeavesOtherCommandsAlone(t *testing.T) {
	ctx := Context{WorkflowID: "order-42", Address: "ns.tmprl.cloud:7233", APIKey: "secret-key"}
	inv, err := ExpandCmd(`echo {workflow_id}`, ctx)
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}
	before := inv

	inv = InjectConnectionFlags(inv, ctx)

	if !reflect.DeepEqual(inv, before) {
		t.Errorf("invocation = %+v, want %+v", inv, before)
	}
}

// hostilePayloads returns values that are each a complete injection for at
// least one quoting context when pasted into shell source.
func hostilePayloads(marker string) map[string]string {
	return map[string]string{
		"command substitution": "$(touch " + marker + ")",
		"backticks":            "`touch " + marker + "`",
		"command separator":    "x; touch " + marker,
		"closes single quote":  "x'; touch " + marker + "; '",
		"closes double quote":  `x"; touch ` + marker + `; "`,
	}
}

func TestExpandCmdHandlesCommentsHeredocsAndLiteralBraces(t *testing.T) {
	ctx := Context{WorkflowID: "order-42", Namespace: "payments", Args: []string{"2"}}

	tests := []struct {
		name     string
		template string
		script   string
		// output is what the command prints; empty skips running it.
		output string
	}{
		{
			name:     "apostrophe in a comment line",
			template: "# don't modify\nprintf '%s' {workflow_id}",
			script:   "# don't modify\nprintf '%s' \"${TEMPO_WORKFLOW_ID}\"",
			output:   "order-42",
		},
		{
			name:     "trailing comment",
			template: "printf '%s' {workflow_id} # it's {not_a_variable}",
			script:   "printf '%s' \"${TEMPO_WORKFLOW_ID}\" # it's {not_a_variable}",
			output:   "order-42",
		},
		{
			name:     "hash inside a word or a quote is not a comment",
			template: `printf '%s' a#b "# it's" '{workflow_id}'`,
			script:   `printf '%s' a#b "# it's" ''"${TEMPO_WORKFLOW_ID}"''`,
			output:   "a#b# it'sorder-42",
		},
		{
			name:     "here-document body",
			template: "cat <<EOF\n\"{workflow_id}\" isn't {namespace}\nEOF\nprintf '%s' {namespace}",
			script:   "cat <<EOF\n\"${TEMPO_WORKFLOW_ID}\" isn't ${TEMPO_NAMESPACE}\nEOF\nprintf '%s' \"${TEMPO_NAMESPACE}\"",
			output:   "\"order-42\" isn't payments\npayments",
		},
		{
			name:     "here-document with stripped tabs",
			template: "cat <<-END\n\t{workflow_id}\n\tEND\nprintf '%s' '{namespace}'",
			script:   "cat <<-END\n\t${TEMPO_WORKFLOW_ID}\n\tEND\nprintf '%s' ''\"${TEMPO_NAMESPACE}\"''",
			output:   "order-42\npayments",
		},
		{
			name:     "arithmetic in a here-document body",
			template: "cat <<EOF\n$(( {1} + 1 ))\nEOF\n",
			script:   "cat <<EOF\n$(( ${TEMPO_ARG_1} + 1 ))\nEOF\n",
			output:   "3\n",
		},
		{
			name:     "shift operator is not a here-document",
			template: "echo $(( 1 << {1} ))\nprintf '%s' {workflow_id}",
			script:   "echo $(( 1 << ${TEMPO_ARG_1} ))\nprintf '%s' \"${TEMPO_WORKFLOW_ID}\"",
			output:   "4\norder-42",
		},
		{
			name:     "here-string is an ordinary word",
			template: `cat <<<{workflow_id}`,
			script:   `cat <<<"${TEMPO_WORKFLOW_ID}"`,
		},
		{
			name:     "escaped brace after a dollar sign in double quotes",
			template: `printf '%s' "$\{HOME} \{x}"`,
			script:   `printf '%s' "\${HOME} {x}"`,
			output:   "${HOME} {x}",
		},
		{
			name:     "escaped brace after a dollar sign unquoted",
			template: `printf '%s' $\{HOME}`,
			script:   `printf '%s' \$\{HOME}`,
			output:   "${HOME}",
		},
		{
			name:     "escaped brace after a dollar sign in single quotes",
			template: `printf '%s' '$\{HOME}'`,
			script:   `printf '%s' '${HOME}'`,
			output:   "${HOME}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := ExpandCmd(tt.template, ctx)
			if err != nil {
				t.Fatalf("ExpandCmd: %v", err)
			}
			if inv.Script != tt.script {
				t.Errorf("script = %q, want %q", inv.Script, tt.script)
			}
			if tt.output == "" {
				return
			}
			out, err := Run(context.Background(), inv)
			if err != nil {
				t.Fatalf("Run: %v\n%s", err, out)
			}
			if out != tt.output {
				t.Errorf("output = %q, want %q", out, tt.output)
			}
		})
	}
}

// The shell expands nothing in a here-document whose delimiter is quoted, so
// a value cannot be delivered there.
func TestExpandCmdRejectsPlaceholdersInLiteralHeredocs(t *testing.T) {
	ctx := Context{WorkflowID: "order-42"}

	for _, template := range []string{
		"cat <<'EOF'\n{workflow_id}\nEOF",
		"cat <<\"EOF\"\n{workflow_id}\nEOF",
		"cat <<\\EOF\n{workflow_id}\nEOF",
		"cat <<''\n{workflow_id}\n\n",
	} {
		_, err := ExpandCmd(template, ctx)
		want := "variable {workflow_id} cannot be used in a here-document with a quoted delimiter"
		if err == nil || err.Error() != want {
			t.Errorf("template %q: error = %v, want %q", template, err, want)
		}
	}

	inv, err := ExpandCmd("cat <<'EOF'\n\\{literal} $(not run)\nEOF\nprintf '%s' {workflow_id}", ctx)
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}
	out, err := Run(context.Background(), inv)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if want := "{literal} $(not run)\norder-42"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}

	// An empty quoted delimiter ends the body at the first empty line.
	inv, err = ExpandCmd("cat <<''\nliteral $(not run)\n\nprintf '%s' {workflow_id}", ctx)
	if err != nil {
		t.Fatalf("ExpandCmd: %v", err)
	}
	if want := "cat <<''\nliteral $(not run)\n\nprintf '%s' \"${TEMPO_WORKFLOW_ID}\""; inv.Script != want {
		t.Errorf("script = %q, want %q", inv.Script, want)
	}
	out, err = Run(context.Background(), inv)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if want := "literal $(not run)\norder-42"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestRunNeverExecutesValuesInHeredocsOrAfterComments(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")

	templates := []struct {
		name     string
		template string
		// suffix is what the command prints after the value.
		suffix string
	}{
		{"here-document body", "cat <<EOF\n{workflow_id}\nEOF\n", "\n"},
		{"substitution in a here-document body", "cat <<EOF\n$(printf '%s' {workflow_id})\nEOF\n", "\n"},
		{"after a comment with an apostrophe", "# don't modify\nprintf '%s' {workflow_id}", ""},
	}

	for _, tt := range templates {
		for valueName, hostile := range hostilePayloads(marker) {
			t.Run(tt.name+"/"+valueName, func(t *testing.T) {
				inv, err := ExpandCmd(tt.template, Context{WorkflowID: hostile})
				if err != nil {
					t.Fatalf("ExpandCmd: %v", err)
				}
				out, err := Run(context.Background(), inv)
				if err != nil {
					t.Fatalf("Run: %v\n%s", err, out)
				}
				if _, statErr := os.Stat(marker); statErr == nil {
					t.Fatalf("value was executed as shell code; output: %q", out)
				}
				if want := hostile + tt.suffix; out != want {
					t.Errorf("output = %q, want %q", out, want)
				}
			})
		}
	}
}
