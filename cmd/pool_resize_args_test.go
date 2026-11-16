package cmd

import (
	"testing"

	"github.com/spf13/pflag"
)

func TestPoolResizeAcceptsNegativeDelta(t *testing.T) {
	if !poolResizeCmd.DisableFlagParsing {
		t.Fatal("pool resize must bypass flag parsing so negative deltas remain positional arguments")
	}
	if err := poolResizeArgs(poolResizeCmd, []string{"-2"}); err != nil {
		t.Fatalf("poolResizeArgs() rejected a negative delta: %v", err)
	}
}

func TestPoolResizeHelpFlagsReturnHelpSentinel(t *testing.T) {
	for _, argument := range []string{"-h", "--help"} {
		if err := poolResizeArgs(poolResizeCmd, []string{argument}); err != pflag.ErrHelp {
			t.Fatalf("poolResizeArgs(%q) error = %v, want %v", argument, err, pflag.ErrHelp)
		}
	}
}
