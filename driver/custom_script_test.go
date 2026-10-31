package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/mcnflag"
)

func TestCustomScriptContents(t *testing.T) {
	tests := map[string]string{
		"empty": "",
		"plain": "#!/bin/bash\ngrowpart /dev/sda 3 || true\n",
	}

	for name, script := range tests {
		t.Run(name, func(t *testing.T) {
			d := &Driver{CustomScript: script}
			if got := string(d.customScriptContents()); got != script {
				t.Fatalf("customScriptContents = %q, want %q", got, script)
			}
		})
	}
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
