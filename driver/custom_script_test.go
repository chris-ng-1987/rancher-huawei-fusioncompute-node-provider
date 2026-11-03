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

// TestDoesNotAdvertiseUserdataFlag guards Rancher's RKE2/K3s provisioning
// path. libmachine skips provision.WithCustomScript when a driver advertises a
// userdata flag, assuming the custom install script was already injected into
// the guest. This driver has no such flag, so Rancher's generated node init
// script is still uploaded and run over SSH after Create().
func TestDoesNotAdvertiseUserdataFlag(t *testing.T) {
	d := NewDriver("n", "/tmp")
	if flag := drivers.DriverUserdataFlag(d); flag != "" {
		t.Fatalf("driver advertises userdata flag %q; libmachine would skip Rancher's RKE2/K3s init script", flag)
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

// TestIsRancherNodeBootstrapContent covers the guard that stops the driver
// from running Rancher's node init/registration script before Rancher does.
func TestIsRancherNodeBootstrapContent(t *testing.T) {
	tests := map[string]struct {
		script           string
		machineBootstrap string
		want             bool
	}{
		"empty": {
			script: "",
			want:   false,
		},
		"host preparation": {
			script: "#!/bin/bash\ngrowpart /dev/sda 3 || true\n",
			want:   false,
		},
		"system agent installer": {
			script: "curl -fL https://rancher.example.com/system-agent-install.sh | sh -s - --server https://rancher.example.com --token abc",
			want:   true,
		},
		"cluster registration command": {
			script: "curl -fL https://rancher.example.com/system-agent-install.sh | sh -s - --token abc --ca-checksum def",
			want:   true,
		},
		"rke2 installer": {
			script: "curl -sfL https://get.rke2.io | INSTALL_RKE2_TYPE=server sh -",
			want:   true,
		},
		"k3s installer": {
			script: "curl -sfL https://get.k3s.io | sh -",
			want:   true,
		},
		"reference to Rancher bootstrap mount": {
			script: rancherMachineBootstrapPath,
			want:   true,
		},
		"identical to mounted machine bootstrap": {
			script:           "#!/bin/sh\ncurl -fL https://rancher.example.com/install.sh | sh -s - --token abc\n",
			machineBootstrap: "#!/bin/sh\ncurl -fL https://rancher.example.com/install.sh | sh -s - --token abc",
			want:             true,
		},
		"different from mounted machine bootstrap": {
			script:           "#!/bin/bash\ngrowpart /dev/sda 3 || true\n",
			machineBootstrap: "#!/bin/sh\ncurl -fL https://rancher.example.com/install.sh | sh -s - --token abc\n",
			want:             false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isRancherNodeBootstrapContent([]byte(tt.script), []byte(tt.machineBootstrap)); got != tt.want {
				t.Fatalf("isRancherNodeBootstrapContent(%q) = %v, want %v", tt.script, got, tt.want)
			}
		})
	}
}
