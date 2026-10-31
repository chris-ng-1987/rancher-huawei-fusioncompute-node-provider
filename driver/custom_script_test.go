package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/mcnflag"
)

func TestCustomScriptContents(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		d := &Driver{}
		got, err := d.customScriptContents()
		if err != nil {
			t.Fatalf("customScriptContents: %v", err)
		}
		if got != nil {
			t.Fatalf("customScriptContents = %q, want nil", got)
		}
	})

	t.Run("inline", func(t *testing.T) {
		script := "#!/bin/bash\ngrowpart /dev/sda 3 || true\n"
		d := &Driver{CustomScript: script}
		got, err := d.customScriptContents()
		if err != nil {
			t.Fatalf("customScriptContents: %v", err)
		}
		if string(got) != script {
			t.Fatalf("customScriptContents = %q, want %q", got, script)
		}
	})

	// A single-line script that looks like a path must not be mistaken for a
	// file. Only the Rancher mount prefix and the explicit file:// prefix are
	// treated as file paths.
	t.Run("inline single line path-like script", func(t *testing.T) {
		d := &Driver{CustomScript: "/bin/true"}
		got, err := d.customScriptContents()
		if err != nil {
			t.Fatalf("customScriptContents: %v", err)
		}
		if string(got) != "/bin/true" {
			t.Fatalf("customScriptContents = %q, want %q", got, "/bin/true")
		}
	})

	t.Run("file prefix", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "customScript")
		script := "#!/bin/bash\necho from-file\n"
		if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}

		d := &Driver{CustomScript: "file://" + path}
		got, err := d.customScriptContents()
		if err != nil {
			t.Fatalf("customScriptContents: %v", err)
		}
		if string(got) != script {
			t.Fatalf("customScriptContents = %q, want %q", got, script)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		d := &Driver{CustomScript: "file://" + filepath.Join(t.TempDir(), "missing")}
		if _, err := d.customScriptContents(); err == nil {
			t.Fatal("expected missing file to be rejected")
		}
	})

	t.Run("Rancher machine-files path is read as a file", func(t *testing.T) {
		d := &Driver{CustomScript: rancherMachineFilesPath + "/customScript"}
		if _, err := d.customScriptContents(); err == nil {
			t.Fatal("expected a missing Rancher machine-files script to be rejected instead of executed as inline text")
		}
	})
}

func TestCustomScriptCommand(t *testing.T) {
	root := &Driver{BaseDriver: &drivers.BaseDriver{}, SSHUser: "root"}
	if got := root.customScriptCommand(); got != "bash -s" {
		t.Fatalf("root command = %q, want %q", got, "bash -s")
	}

	user := &Driver{BaseDriver: &drivers.BaseDriver{}, SSHUser: "rancher"}
	if got := user.customScriptCommand(); got != "sudo -n bash -s" {
		t.Fatalf("non-root command = %q, want %q", got, "sudo -n bash -s")
	}
}

func TestCustomScriptFlagDefault(t *testing.T) {
	d := NewDriver("n", "/tmp")
	var found bool

	for _, flag := range d.GetCreateFlags() {
		if f, ok := flag.(mcnflag.StringFlag); ok && f.Name == "fusioncompute-custom-script" {
			found = true
			if f.Default() != "" {
				t.Errorf("fusioncompute-custom-script default = %#v, want empty", f.Default())
			}
		}
	}
	if !found {
		t.Fatal("fusioncompute-custom-script flag not found")
	}
}

func TestSetConfigFromFlagsCustomScript(t *testing.T) {
	script := "#!/bin/bash\necho hello\n"
	d := NewDriver("n", "/tmp")
	opts := fakeDriverOptions{
		"fusioncompute-dcs-hostname":   "dcs.example.com",
		"fusioncompute-dcs-username":   "admin",
		"fusioncompute-dcs-password":   "secret",
		"fusioncompute-dcs-site-uri":   "/service/sites/1",
		"fusioncompute-template-id":    "tpl",
		"fusioncompute-port-group-urn": "urn:pg",
		"fusioncompute-datastore-urn":  "urn:ds",
		"fusioncompute-custom-script":  script,
	}

	if err := d.SetConfigFromFlags(opts); err != nil {
		t.Fatalf("SetConfigFromFlags: %v", err)
	}
	if d.CustomScript != script {
		t.Fatalf("CustomScript = %q, want %q", d.CustomScript, script)
	}
}

// TestCustomScriptNotPersisted guards the json:"-" tag. The script is part of
// the Rancher machine config, but it must not be duplicated into the machine
// state file on disk.
func TestCustomScriptNotPersisted(t *testing.T) {
	d := NewDriver("n", "/tmp")
	d.CustomScript = "echo super-secret"

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), d.CustomScript) {
		t.Fatal("custom script contents must not be serialized into the machine state")
	}
}

func TestRunCustomScriptSkipsEmpty(t *testing.T) {
	d := NewDriver("n", "/tmp")
	if err := d.runCustomScript(); err != nil {
		t.Fatalf("empty custom script must be a no-op, got %v", err)
	}
}
