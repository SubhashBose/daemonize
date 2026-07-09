//go:build !windows

package daemon

import (
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cmds := &Commands{
		Start: "start", WatchStart: "watch-start", Stop: "stop",
		Restart: "restart", Reload: "reload", Status: "status",
	}

	tests := []struct {
		name        string
		args        []string
		mustBeFirst bool
		wantCommand string
		wantRest    []string
	}{
		// Default (scan-from-end): RouteMUX-style, command after the flags.
		{
			name:        "trailing command found by default",
			args:        []string{"--config", "c.yml", "start"},
			mustBeFirst: false,
			wantCommand: "start",
			wantRest:    []string{"--config", "c.yml"},
		},
		{
			name:        "no command leaves args untouched",
			args:        []string{"--config", "c.yml"},
			mustBeFirst: false,
			wantCommand: "",
			wantRest:    []string{"--config", "c.yml"},
		},
		// CommandMustBeFirst: daemonize-style, wrapping an arbitrary program.
		{
			name:        "first command recognized",
			args:        []string{"start", "sleep", "300"},
			mustBeFirst: true,
			wantCommand: "start",
			wantRest:    []string{"sleep", "300"},
		},
		{
			name:        "trailing target arg is not mistaken for a command",
			args:        []string{"start", "sleep", "300", "stop"},
			mustBeFirst: true,
			wantCommand: "start",
			wantRest:    []string{"sleep", "300", "stop"},
		},
		{
			name:        "command after flags is rejected when it must be first",
			args:        []string{"--config", "c.yml", "start"},
			mustBeFirst: true,
			wantCommand: "",
			wantRest:    []string{"--config", "c.yml", "start"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCommand, gotRest := parseArgs(tt.args, cmds, tt.mustBeFirst)
			if gotCommand != tt.wantCommand {
				t.Errorf("command = %q, want %q", gotCommand, tt.wantCommand)
			}
			if !reflect.DeepEqual(gotRest, tt.wantRest) {
				t.Errorf("rest = %#v, want %#v", gotRest, tt.wantRest)
			}
		})
	}
}
