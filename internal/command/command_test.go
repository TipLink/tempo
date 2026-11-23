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
			display:  `echo \{workflow_id} order-42`,
			env:      []string{"TEMPO_WORKFLOW_ID=order-42"},
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
	// Each value is a complete injection for at least one quoting context when
	// pasted into shell source.
	hostileValues := map[string]string{
		"command substitution": "$(touch " + marker + ")",
		"backticks":            "`touch " + marker + "`",
		"command separator":    "x; touch " + marker,
		"closes single quote":  "x'; touch " + marker + "; '",
		"closes double quote":  `x"; touch ` + marker + `; "`,
	}

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
		{"inside command substitution", `echo "$(printf '%s' {workflow_id})"`, false},
		{"inside backticks", "echo `printf '%s' {workflow_id}`", false},
	}

	for _, tt := range templates {
		for valueName, hostile := range hostileValues {
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
