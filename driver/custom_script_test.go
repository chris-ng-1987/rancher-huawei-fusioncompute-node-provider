package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/mcnflag"
)

func TestCustomScriptContents(t *testing.T) {
	tests := map[string]struct {
		script string
		base64 bool
		want   string
	}{
		"empty": {
			script: "",
			want:   "",
		},
		"plain": {
			script: "#!/bin/bash\ngrowpart /dev/sda 3 || true\n",
			want:   "#!/bin/bash\ngrowpart /dev/sda 3 || true\n",
		},
		"base64": {
			script: base64.StdEncoding.EncodeToString([]byte("growpart /dev/sda 3\nexit 0\n")),
			base64: true,
			want:   "growpart /dev/sda 3\nexit 0\n",
		},
		"wrapped base64": {
			script: "Z3Jvd3BhcnQgL2Rldi9zZGEgMwpleGl0\nIDAK\n",
			base64: true,
			want:   "growpart /dev/sda 3\nexit 0\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d := &Driver{CustomScript: tt.script, CustomScriptBase64: tt.base64}
			got, err := d.customScriptContents()
			if err != nil {
				t.Fatalf("customScriptContents: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("customScriptContents = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("invalid base64", func(t *testing.T) {
		d := &Driver{CustomScript: "not-base64!", CustomScriptBase64: true}
		if _, err := d.customScriptContents(); err == nil {
			t.Fatal("expected invalid base64 to be rejected")
		}
	})
}

func TestValidateCustomScriptBase64(t *testing.T) {
	base := func() *Driver {
		return &Driver{
			DCSHostname:  "dcs.example.com",
			DCSUsername:  "admin",
			DCSPassword:  "secret",
			DCSSiteURI:   "/service/sites/1",
			TemplateID:   "tpl",
			PortGroupURN: "urn:pg",
			DatastoreURN: "urn:ds",
		}
	}

	valid := base()
	valid.CustomScript = base64.StdEncoding.EncodeToString([]byte("echo ok\n"))
	valid.CustomScriptBase64 = true
	if err := valid.validate(); err != nil {
		t.Fatalf("valid base64 script should validate, got %v", err)
	}

	invalid := base()
	invalid.CustomScript = "not base64!"
	invalid.CustomScriptBase64 = true
	if err := invalid.validate(); err == nil {
		t.Fatal("invalid base64 script should be rejected by validate")
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

func TestCustomScriptFlagDefaults(t *testing.T) {
	d := NewDriver("n", "/tmp")
	var sawScript, sawBase64 bool

	for _, flag := range d.GetCreateFlags() {
		switch f := flag.(type) {
		case mcnflag.StringFlag:
			if f.Name == "fusioncompute-custom-script" {
				sawScript = true
				if f.Default() != "" {
					t.Errorf("fusioncompute-custom-script default = %#v, want empty", f.Default())
				}
			}
		case mcnflag.BoolFlag:
			if f.Name == "fusioncompute-custom-script-base64" {
				sawBase64 = true
				if got, ok := f.Default().(bool); !ok || got {
					t.Errorf("fusioncompute-custom-script-base64 default = %#v, want false", f.Default())
				}
			}
		}
	}
	if !sawScript {
		t.Fatal("fusioncompute-custom-script flag not found")
	}
	if !sawBase64 {
		t.Fatal("fusioncompute-custom-script-base64 flag not found")
	}
}

func TestSetConfigFromFlagsCustomScript(t *testing.T) {
	plain := "#!/bin/bash\necho hello\n"
	script := base64.StdEncoding.EncodeToString([]byte(plain))
	d := NewDriver("n", "/tmp")
	opts := fakeDriverOptions{
		"fusioncompute-dcs-hostname":         "dcs.example.com",
		"fusioncompute-dcs-username":         "admin",
		"fusioncompute-dcs-password":         "secret",
		"fusioncompute-dcs-site-uri":         "/service/sites/1",
		"fusioncompute-template-id":          "tpl",
		"fusioncompute-port-group-urn":       "urn:pg",
		"fusioncompute-datastore-urn":        "urn:ds",
		"fusioncompute-custom-script":        script,
		"fusioncompute-custom-script-base64": true,
	}

	if err := d.SetConfigFromFlags(opts); err != nil {
		t.Fatalf("SetConfigFromFlags: %v", err)
	}
	if d.CustomScript != script {
		t.Fatalf("CustomScript = %q, want %q", d.CustomScript, script)
	}
	if !d.CustomScriptBase64 {
		t.Fatal("CustomScriptBase64 = false, want true")
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

func TestRunCustomScriptRejectsInvalidBase64(t *testing.T) {
	d := NewDriver("n", "/tmp")
	d.CustomScript = "not base64!"
	d.CustomScriptBase64 = true

	if err := d.runCustomScript(); err == nil {
		t.Fatal("invalid base64 must fail before attempting SSH")
	}
}
