package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// privateCommand writes an executable owned by the test user in a private
// directory.
func privateCommand(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(privateDir(t), "bin")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cmd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// privateDir is a new directory under the home directory: the shared
// temporary directory may itself be group or world writable.
func privateDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".cue-config-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func appendConfig(t *testing.T, loc Location, text string) {
	t.Helper()
	f, err := os.OpenFile(loc.Path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestReplyAdaptersAndEventHooksLoad(t *testing.T) {
	loc, _ := setupConfig(t)
	adapter, hook := privateCommand(t, 0700), privateCommand(t, 0755)
	appendConfig(t, loc, "\n[reply_adapters.factory]\ncommand = \""+adapter+"\"\ntimeout = \"45s\"\n\n[event_hooks.factory]\ncommand = \""+hook+"\"\nchannels = [\"C0BAQM6JEAJ\"]\n")
	c, err := Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	a, h := c.ReplyAdapters["factory"], c.EventHooks["factory"]
	if a.Command != adapter || a.TimeoutDuration() != 45*time.Second || h.Command != hook || h.TimeoutDuration() != DefaultEventHookTimeout {
		t.Fatalf("config = %#v %#v", a, h)
	}
	if !h.Wants("C0BAQM6JEAJ") || h.Wants("C0OTHER") || !(EventHook{}).Wants("C0ANY") {
		t.Fatal("channel filter")
	}
	view := Redact(c)
	if view.ReplyAdapters["factory"].Command != adapter || view.EventHooks["factory"].Channels[0] != "C0BAQM6JEAJ" {
		t.Fatalf("view = %#v", view)
	}
	// config set keeps the tables.
	if _, err := Set(loc, "sqlite_busy_timeout", "6s"); err != nil {
		t.Fatal(err)
	}
	if again, err := Load(loc); err != nil || again.ReplyAdapters["factory"].Command != adapter {
		t.Fatalf("after set = %#v, %v", again.ReplyAdapters, err)
	}
}

func TestCommandValidation(t *testing.T) {
	good := privateCommand(t, 0700)
	for name, tc := range map[string]struct{ table, want string }{
		"relative":      {"[reply_adapters.factory]\ncommand = \"bin/reply\"\n", "absolute"},
		"unclean":       {"[reply_adapters.factory]\ncommand = \"/tmp/../" + strings.TrimPrefix(good, "/") + "\"\n", "absolute"},
		"bad name":      {"[reply_adapters.Factory]\ncommand = \"" + good + "\"\n", "name"},
		"long timeout":  {"[reply_adapters.factory]\ncommand = \"" + good + "\"\ntimeout = \"10m\"\n", "timeout"},
		"short timeout": {"[event_hooks.factory]\ncommand = \"" + good + "\"\ntimeout = \"10ms\"\n", "timeout"},
		"bad channel":   {"[event_hooks.factory]\ncommand = \"" + good + "\"\nchannels = [\"D0DIRECT\"]\n", "channel"},
		"twice":         {"[event_hooks.factory]\ncommand = \"" + good + "\"\nchannels = [\"C0A\", \"C0A\"]\n", "channel"},
		"unknown key":   {"[event_hooks.factory]\ncommand = \"" + good + "\"\nshell = true\n", "unknown key"},
		"missing":       {"[reply_adapters.factory]\ncommand = \"" + filepath.Join(filepath.Dir(good), "missing") + "\"\n", "no such file"},
		"group write":   {"[reply_adapters.factory]\ncommand = \"" + privateCommand(t, 0770) + "\"\n", "group/world writable"},
		"not exec":      {"[reply_adapters.factory]\ncommand = \"" + privateCommand(t, 0600) + "\"\n", "not executable"},
	} {
		t.Run(name, func(t *testing.T) {
			loc, _ := setupConfig(t)
			appendConfig(t, loc, "\n"+tc.table)
			if _, err := Load(loc); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCommandMustNotBeASymlinkOrInAnOpenDirectory(t *testing.T) {
	good := privateCommand(t, 0700)
	link := filepath.Join(filepath.Dir(good), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(privateDir(t), "open")
	if err := os.Mkdir(open, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0777); err != nil {
		t.Fatal(err)
	}
	inOpen := filepath.Join(open, "cmd")
	if err := os.WriteFile(inOpen, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, inOpen} {
		loc, _ := setupConfig(t)
		appendConfig(t, loc, "\n[event_hooks.factory]\ncommand = \""+path+"\"\n")
		if _, err := Load(loc); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}
