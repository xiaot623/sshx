package sshcompat

import (
	"reflect"
	"testing"
)

func TestParseFindsTargetAndRemoteCommand(t *testing.T) {
	p := Parse([]string{"-J", "jump", "-o", "StrictHostKeyChecking=no", "user@example.com", "uname", "-a"})
	if p.Target != "user@example.com" {
		t.Fatalf("target = %q", p.Target)
	}
	if !reflect.DeepEqual(p.RemoteCommand, []string{"uname", "-a"}) {
		t.Fatalf("remote command = %#v", p.RemoteCommand)
	}
}

func TestParseNoWrapIsRemovedBeforeDelegation(t *testing.T) {
	p := Parse([]string{"--no-wrap", "-p", "2222", "remote"})
	if !p.NoWrap {
		t.Fatal("NoWrap = false")
	}
	if !reflect.DeepEqual(p.Args, []string{"-p", "2222", "remote"}) {
		t.Fatalf("delegated args = %#v", p.Args)
	}
}

func TestParseInfoMode(t *testing.T) {
	for _, args := range [][]string{{"-V"}, {"-G", "remote"}, {"-Q", "cipher"}} {
		if p := Parse(args); !p.InfoMode {
			t.Fatalf("%#v did not parse as info mode", args)
		}
	}
}
