package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/state"
)

// TestBuildCloneSpec asserts that buildCloneSpec() populates the JSON
// payload with the values we set on the Driver. This is a pure unit test
// so it can run without a live FusionCompute host.
func TestBuildCloneSpec(t *testing.T) {
	d := &Driver{
		VMName:        "test-vm-01",
		VMDescription: "unit test",
		CPUCount:      4,
		MemoryMB:      8192,
		DiskGB:        100,
		PortGroupURN:  "urn:test:pg",
		DatastoreURN:  "urn:test:ds",
	}

	spec := d.buildCloneSpec()

	if spec.Name != "test-vm-01" {
		t.Errorf("expected Name=test-vm-01, got %q", spec.Name)
	}
	// cpu/memory numerics must be marshaled as JSON strings — the
	// Ansible playbook's `to_json` filter produces strings for
	// templated values, and FC's schema validator accepts (in fact
	// appears to require) the string form on this build.
	if spec.VMConfig.CPU.Quantity != "4" {
		t.Errorf("expected CPU.Quantity=\"4\" (string), got %q", spec.VMConfig.CPU.Quantity)
	}
	if spec.VMConfig.Memory.QuantityMB != "8192" {
		t.Errorf("expected Memory.QuantityMB=\"8192\" (string), got %q", spec.VMConfig.Memory.QuantityMB)
	}
	if spec.VMConfig.Memory.Weight != "81920" {
		t.Errorf("expected Memory.Weight=\"81920\" (string), got %q", spec.VMConfig.Memory.Weight)
	}
	if len(spec.VMConfig.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(spec.VMConfig.Disks))
	}
	if spec.VMConfig.Disks[0].QuantityGB != 100 {
		t.Errorf("expected disk size 100 GB, got %d", spec.VMConfig.Disks[0].QuantityGB)
	}
	if spec.VMConfig.Disks[0].DatastoreURN != "urn:test:ds" {
		t.Errorf("expected datastore URN urn:test:ds, got %q", spec.VMConfig.Disks[0].DatastoreURN)
	}
	if len(spec.VMConfig.NICs) != 1 {
		t.Fatalf("expected 1 NIC, got %d", len(spec.VMConfig.NICs))
	}
	if spec.VMConfig.NICs[0].PortGroupURN != "urn:test:pg" {
		t.Errorf("expected NIC port-group urn:test:pg, got %q", spec.VMConfig.NICs[0].PortGroupURN)
	}
	if spec.VMCustomization.Hostname != "test-vm-01" {
		t.Errorf("expected customization hostname=test-vm-01, got %q", spec.VMCustomization.Hostname)
	}
	if spec.VMCustomization.OSType != "Linux" {
		t.Errorf("expected osType=Linux, got %q", spec.VMCustomization.OSType)
	}
	// vmcustomizationId must be JSON null — FC rejects "" with
	// errorCode 10000001 ("请求消息格式非法").
	if spec.VMCustomization.VMCustomizationID != nil {
		t.Errorf("expected vmcustomizationId=nil, got %#v", spec.VMCustomization.VMCustomizationID)
	}
	// vmConfig.properties is REQUIRED and must carry three explicit
	// null sub-fields, exactly like the working playbook body. The
	// previous (omitted-pointer) approach was rejected by FC's
	// schema validator.
	if spec.VMConfig.Properties.BootOptions != nil ||
		spec.VMConfig.Properties.IsEnableHA != nil ||
		spec.VMConfig.Properties.VMFaultProcess != nil {
		t.Errorf("expected properties.{bootOptions,isEnableHa,vmFaultProcess}=nil, got %+v", spec.VMConfig.Properties)
	}
	// group is always set to "" — mirrors the working playbook.
	if spec.Group != "" {
		t.Errorf("expected group=\"\", got %q", spec.Group)
	}
	// Password is always set to "" — the reference body always sends the
	// field, empty when no reset is requested. With IsUpdateVmPassword=false
	// FC does NOT reset the root account — the new VM keeps the
	// template's baked-in root password.
	if spec.VMCustomization.Password != "" {
		t.Errorf("expected empty customization password, got %q", spec.VMCustomization.Password)
	}
	if spec.VMCustomization.IsUpdateVMPassword {
		t.Errorf("expected IsUpdateVmPassword=false so the empty password is a no-op (do NOT update)")
	}
	if len(spec.VMCustomization.NICSpecification) != 1 {
		t.Fatalf("expected 1 nicSpecification, got %d", len(spec.VMCustomization.NICSpecification))
	}
	ns := spec.VMCustomization.NICSpecification[0]
	if ns.IPVersion != 4 || ns.SequenceNum != 1 {
		t.Errorf("NIC spec must be {ipVersion:4, sequenceNum:1}, got %+v", ns)
	}
}

// TestBuildCloneSpecDHCP verifies the clone spec always produces a single
// bare nicSpecification entry `{ipVersion:4, sequenceNum:1}` so the guest
// obtains its address via DHCP. FC rejects an empty/absent nicSpecification
// on this build, so DHCP must still send one entry rather than `[]`.
func TestBuildCloneSpecDHCP(t *testing.T) {
	d := &Driver{
		VMName:       "dhcp-vm-01",
		CPUCount:     2,
		MemoryMB:     4096,
		DiskGB:       100,
		PortGroupURN: "urn:test:pg",
		DatastoreURN: "urn:test:ds",
	}

	spec := d.buildCloneSpec()

	if len(spec.VMCustomization.NICSpecification) != 1 {
		t.Fatalf("expected 1 nicSpecification entry for DHCP, got %d", len(spec.VMCustomization.NICSpecification))
	}
	ns := spec.VMCustomization.NICSpecification[0]
	if ns.IPVersion != 4 || ns.SequenceNum != 1 {
		t.Errorf("DHCP NIC spec must be {ipVersion:4, sequenceNum:1}, got %+v", ns)
	}

	buf, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(buf)

	if !strings.Contains(s, `"nicSpecification":[{"ipVersion":4,"sequenceNum":1}]`) {
		t.Errorf(`DHCP payload must contain "nicSpecification":[{"ipVersion":4,"sequenceNum":1}], got: %s`, s)
	}
	if strings.Contains(s, `"nicSpecification":null`) {
		t.Errorf(`DHCP payload must not send "nicSpecification":null, got: %s`, s)
	}
	if strings.Contains(s, `"nicSpecification":[]`) {
		t.Errorf(`DHCP payload must not send an empty "nicSpecification":[], got: %s`, s)
	}
}

// TestVMGuestIP verifies the DHCP address is read from
// vmConfig.nics[0].ip — FC's GET {vmURI} response has no usable top-level
// `ip` on this build, so the NIC spec is the source of truth.
func TestVMGuestIP(t *testing.T) {
	body := []byte(`{
		"uri": "/service/sites/1/vms/i-000001E2",
		"name": "test-vm-01",
		"status": "running",
		"ip": "",
		"vmConfig": {
			"nics": [
				{"name": "Network Adapter 0", "ip": "192.0.2.10", "sequenceNum": 0}
			]
		}
	}`)
	var vm VM
	if err := json.Unmarshal(body, &vm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := vm.GuestIP(); got != "192.0.2.10" {
		t.Errorf("GuestIP()=%q, want 192.0.2.10", got)
	}

	// Prefer the primary NIC even if a secondary one is listed first.
	mixed := []byte(`{"vmConfig":{"nics":[{"ip":"10.0.0.2","sequenceNum":1},{"ip":"10.0.0.3","sequenceNum":0}]}}`)
	var vm2 VM
	if err := json.Unmarshal(mixed, &vm2); err != nil {
		t.Fatalf("unmarshal mixed: %v", err)
	}
	if got := vm2.GuestIP(); got != "10.0.0.3" {
		t.Errorf("GuestIP()=%q, want primary NIC 10.0.0.3", got)
	}

	// No address yet (guest still booting) -> "".
	empty := []byte(`{"vmConfig":{"nics":[{"ip":"","sequenceNum":0}]}}`)
	var vm3 VM
	if err := json.Unmarshal(empty, &vm3); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if got := vm3.GuestIP(); got != "" {
		t.Errorf("GuestIP()=%q, want empty", got)
	}

	// FC reports 0.0.0.0 until DHCP has assigned an address; that must not
	// be accepted as the guest IP (otherwise SSH dials 0.0.0.0).
	zero := []byte(`{"vmConfig":{"nics":[{"ip":"0.0.0.0","sequenceNum":0}]}}`)
	var vm4 VM
	if err := json.Unmarshal(zero, &vm4); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if got := vm4.GuestIP(); got != "" {
		t.Errorf("GuestIP()=%q, want empty for the 0.0.0.0 placeholder", got)
	}

	// Skip a 0.0.0.0 primary NIC in favour of a NIC that does have an address.
	zeroThenReal := []byte(`{"vmConfig":{"nics":[{"ip":"0.0.0.0","sequenceNum":0},{"ip":"10.0.0.9","sequenceNum":1}]}}`)
	var vm5 VM
	if err := json.Unmarshal(zeroThenReal, &vm5); err != nil {
		t.Fatalf("unmarshal zeroThenReal: %v", err)
	}
	if got := vm5.GuestIP(); got != "10.0.0.9" {
		t.Errorf("GuestIP()=%q, want 10.0.0.9 (0.0.0.0 primary skipped)", got)
	}
}

// TestValidate exercises the required-field check without making API calls.
func TestValidate(t *testing.T) {
	d := &Driver{}
	if err := d.validate(); err == nil {
		t.Fatal("expected validation error on empty driver, got nil")
	}

	d.DCSHostname = "dcs.example.com"
	d.DCSUsername = "admin"
	d.DCSPassword = "secret"
	d.TemplateID = "tpl"
	d.PortGroupURN = "urn:pg"
	d.DatastoreURN = "urn:ds"
	if err := d.validate(); err != nil {
		t.Fatalf("expected validation to pass with required fields set, got %v", err)
	}
}

// TestBuildCloneSpecJSONShape round-trips the payload through
// encoding/json and asserts that the on-the-wire shape matches the
// reference playbook (the reference provisioning workflow) AND the
// real-world working body captured on the FC farm. This catches
// regressions where a future struct change accidentally drops a
// required field, changes a type (e.g. int → string), or emits ""
// where null is required — all of which trip FC's strict schema
// validator with errorCode 10000001 ("请求消息格式非法").
func TestBuildCloneSpecJSONShape(t *testing.T) {
	d := &Driver{
		VMName:        "test-vm-01",
		VMDescription: "unit test",
		Location:      "urn:loc",
		CPUCount:      2, MemoryMB: 4096, DiskGB: 100,
		PortGroupURN: "urn:pg", DatastoreURN: "urn:ds",
	}
	buf, err := json.Marshal(d.buildCloneSpec())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(buf)

	// Required fields and value types from the working playbook body.
	for _, want := range []string{
		`"name":"test-vm-01"`,
		`"description":"unit test"`,
		`"group":""`, // playbook always sends ""
		`"location":"urn:loc"`,
		`"isBindingHost":"false"`, // string, not bool
		`"autoBoot":true`,
		`"quantity":"2"`, // cpu.quantity as STRING
		`"coresPerSocket":1`,
		`"reservation":0`,
		`"weight":"1000"`, // cpu.weight as STRING
		`"limit":0`,
		`"cpuHotPlug":1`,
		`"cpuPolicy":"shared"`,
		`"cpuThreadPolicy":"prefer"`,
		`"quantityMB":"4096"`, // memory.quantityMB as STRING
		`"hugePage":"4K"`,
		`"memHotPlug":1`,
		`"datastoreUrn":"urn:ds"`,
		`"quantityGB":100`,
		`"sequenceNum":1`,
		`"pciType":"VIRTIO"`,
		`"bootOrder":1`,
		`"encrypted":"0"`,
		`"storageProtocol":"0"`,
		`"portGroupUrn":"urn:pg"`,
		`"virtIo":1`,
		`"bootOrder":3`,
		`"connectAtPowerOn":true`,
		// properties MUST be present with three explicit nulls.
		`"properties":{"bootOptions":null,"isEnableHa":null,"vmFaultProcess":null}`,
		`"osType":"Linux"`,
		`"updatePassword":false`,
		`"vmcustomizationId":null`, // null, not ""
		`"isUpdateVmPassword":false`,
		`"nicSpecification":[{"ipVersion":4,"sequenceNum":1}]`,
		`"hostname":"test-vm-01"`,
		`"password":""`, // reference body always sends this; empty when no reset is requested
	} {
		if !strings.Contains(s, want) {
			t.Errorf("clone payload missing expected substring %s\nfull payload: %s", want, s)
		}
	}

	// Sanity: vmcustomizationId must NOT be the empty string.
	if strings.Contains(s, `"vmcustomizationId":""`) {
		t.Errorf(`clone payload has vmcustomizationId="" instead of null (FC rejects with errorCode 10000001)\nfull payload: %s`, s)
	}
	// Sanity: cpu/memory numerics must NOT be emitted as bare JSON
	// numbers (the working body always quotes them).
	for _, mustNotAppear := range []string{
		`"quantity":2,`, `"weight":1000,`,
		`"quantityMB":4096,`, `"weight":40960,`,
	} {
		if strings.Contains(s, mustNotAppear) {
			t.Errorf("clone payload has unquoted numeric %s; FC expects string form\nfull payload: %s", mustNotAppear, s)
		}
	}
	// Sanity: properties MUST be present (omitting it was a previous
	// 10000001 cause).
	if !strings.Contains(s, `"properties"`) {
		t.Errorf("clone payload missing \"properties\" block; FC requires it with null sub-fields (errorCode 10000001)\nfull payload: %s", s)
	}
}

// TestBuildCloneSpecLocationOmitted verifies that when the user does
// not supply --fusioncompute-location, the `location` field is OMITTED
// from the JSON (omitempty). This is the one optional driver input
// that we genuinely omit — `description`, `group`, and `password` are
// always sent (as "" or the description flag's value) because the
// working playbook always sends them.
func TestBuildCloneSpecLocationOmitted(t *testing.T) {
	d := &Driver{
		VMName: "minimal",
		// Location intentionally empty.
		CPUCount: 2, MemoryMB: 4096, DiskGB: 100,
		PortGroupURN: "urn:pg", DatastoreURN: "urn:ds",
	}
	buf, err := json.Marshal(d.buildCloneSpec())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(buf)
	if strings.Contains(s, `"location"`) {
		t.Errorf("empty Location should be omitted from JSON; full payload: %s", s)
	}
	// These fields are ALWAYS present per the working playbook.
	for _, must := range []string{`"description":`, `"group":`, `"password":`} {
		if !strings.Contains(s, must) {
			t.Errorf("required field %s must be present in JSON (playbook always sends it); full payload: %s", must, s)
		}
	}
}

// TestBuildCloneSpecVMNameFallback verifies that an unset VMName is
// replaced with BaseDriver.MachineName so the cloned VM has a
// non-empty name (FC rejects "" with errorCode 10000001). This
// exercises the docker-machine plugin path where NewDriver() is not
// called and VMName stays at its zero value.
func TestBuildCloneSpecVMNameFallback(t *testing.T) {
	d := &Driver{
		BaseDriver: &drivers.BaseDriver{MachineName: "test-docker-machine"},
		// VMName deliberately empty.
		CPUCount: 2, MemoryMB: 4096, DiskGB: 100,
		PortGroupURN: "urn:pg", DatastoreURN: "urn:ds",
	}
	spec := d.buildCloneSpec()
	if spec.Name != "test-docker-machine" {
		t.Errorf("expected VMName fall-back to MachineName, got Name=%q", spec.Name)
	}
	if spec.VMCustomization.Hostname != "test-docker-machine" {
		t.Errorf("expected Hostname fall-back to MachineName, got %q", spec.VMCustomization.Hostname)
	}
}

// TestMapState covers the state-coercion helper.
func TestMapState(t *testing.T) {
	d := &Driver{}
	cases := []struct {
		power, status string
		want          state.State
	}{
		{"running", "running", state.Running},
		{"stopped", "stopped", state.Stopped},
		{"", "creating", state.Starting},
		{"", "starting", state.Starting},
		{"weird", "weird", state.None},
	}
	for _, c := range cases {
		got := d.mapState(c.status, c.power)
		if got != c.want {
			t.Errorf("mapState(status=%q,power=%q)=%v, want %v", c.status, c.power, got, c.want)
		}
	}
}

// TestTaskDecodingCloneResponse verifies that the JSON returned by
// FusionCompute's POST /vms/{template}/action/clone decodes into a
// Task with both the new VM URI (uri) and the task tracker
// (taskUri) populated. Previously we conflated the two — polling
// `task.URI` (the VM) instead of `task.TaskURI` caused the driver to
// spin forever GET-ing the VM URN.
func TestTaskDecodingCloneResponse(t *testing.T) {
	body := []byte(`{
		"uri": "/service/sites/1/vms/i-000001B0",
		"taskUri": "/service/sites/1/tasks/abc-123",
		"status": "running",
		"progress": 0
	}`)
	var t1 Task
	if err := json.Unmarshal(body, &t1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if t1.URI != "/service/sites/1/vms/i-000001B0" {
		t.Errorf("URI = %q, want VM URN", t1.URI)
	}
	if t1.TaskURI != "/service/sites/1/tasks/abc-123" {
		t.Errorf("TaskURI = %q, want task tracker", t1.TaskURI)
	}
	// Test that polling response (which has no `uri` for the VM)
	// still decodes correctly — `uri` is omitempty.
	pollBody := []byte(`{
		"status": "success",
		"progress": 100,
		"result": "/service/sites/1/vms/i-000001B0"
	}`)
	var t2 Task
	if err := json.Unmarshal(pollBody, &t2); err != nil {
		t.Fatalf("unmarshal poll response: %v", err)
	}
	if t2.Status != "success" || t2.Progress != 100 {
		t.Errorf("poll response: status=%q progress=%d, want success/100", t2.Status, t2.Progress)
	}
}

func TestNewDriverAndMachineID(t *testing.T) {
	d := NewDriver("node-1", "/tmp/store")
	if d.DriverName() != DriverName {
		t.Fatalf("DriverName=%q", d.DriverName())
	}
	if d.GetSSHUsername() != "root" {
		t.Fatalf("SSH user=%q", d.GetSSHUsername())
	}
	d.VMMoRef = "/service/sites/1/vms/i-1"
	id, err := d.GetMachineId()
	if err != nil || id != d.VMMoRef {
		t.Fatalf("GetMachineId=%q err=%v", id, err)
	}
	if err := d.Upgrade(); err == nil {
		t.Fatal("Upgrade should be unsupported")
	}
}

func TestGetURLJoinHostPort(t *testing.T) {
	d := NewDriver("n", "/tmp")
	d.IPAddr = "10.1.2.3"
	u, err := d.GetURL()
	if err != nil || u != "tcp://10.1.2.3:2376" {
		t.Fatalf("url=%q err=%v", u, err)
	}
	d.IPAddr = "2001:db8::1"
	u, err = d.GetURL()
	if err != nil || u != "tcp://[2001:db8::1]:2376" {
		t.Fatalf("ipv6 url=%q err=%v", u, err)
	}
}

func TestPublicSSHKeyPath(t *testing.T) {
	d := NewDriver("n", "/tmp/fc-machine")
	d.SSHKeyPath = "/tmp/id_rsa"
	if d.publicSSHKeyPath() != "/tmp/id_rsa.pub" {
		t.Fatalf("got %q", d.publicSSHKeyPath())
	}
}

// TestSSHPortFixed verifies the SSH port is always 22, regardless of any
// value a legacy saved config may carry: there is no
// --fusioncompute-ssh-port flag.
func TestSSHPortFixed(t *testing.T) {
	d := NewDriver("n", "/tmp")
	d.SSHPort = 2222
	if p, err := d.GetSSHPort(); err != nil || p != DefaultSSHPort {
		t.Fatalf("GetSSHPort = %d, err=%v; want %d", p, err, DefaultSSHPort)
	}

	raw := []byte(`{"SSHPort":2222,"MachineName":"n"}`)
	if err := json.Unmarshal(raw, d); err != nil {
		t.Fatal(err)
	}
	if d.SSHPort != DefaultSSHPort {
		t.Fatalf("unmarshaled SSHPort = %d, want %d", d.SSHPort, DefaultSSHPort)
	}
	if p, err := d.GetSSHPort(); err != nil || p != DefaultSSHPort {
		t.Fatalf("GetSSHPort after unmarshal = %d, err=%v; want %d", p, err, DefaultSSHPort)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	d := NewDriver("n", "/tmp")
	raw := []byte(`{"DCSHostname":"dcs.example.com","DCSUsername":"admin","MachineName":"n"}`)
	if err := json.Unmarshal(raw, d); err != nil {
		t.Fatal(err)
	}
	if d.DCSHostname != "dcs.example.com" || d.DCSUsername != "admin" {
		t.Fatalf("unmarshaled %+v", d)
	}
}

// TestGenerateRandomPassword verifies length, per-class coverage, allowed
// charset, and that two calls differ.
func TestGenerateRandomPassword(t *testing.T) {
	a, err := generateRandomPassword(generatedPasswordLength)
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateRandomPassword(generatedPasswordLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != generatedPasswordLength {
		t.Fatalf("len=%d, want %d", len(a), generatedPasswordLength)
	}
	if a == b {
		t.Errorf("two generated passwords are identical (%q)", a)
	}

	var hasUpper, hasLower, hasDigit, hasSymbol bool
	for _, r := range a {
		switch {
		case strings.ContainsRune(passwordUpperChars, r):
			hasUpper = true
		case strings.ContainsRune(passwordLowerChars, r):
			hasLower = true
		case strings.ContainsRune(passwordDigitChars, r):
			hasDigit = true
		case strings.ContainsRune(passwordSymbolChars, r):
			hasSymbol = true
		default:
			t.Errorf("password %q contains a character outside the allowed classes: %q", a, r)
		}
	}
	if !(hasUpper && hasLower && hasDigit && hasSymbol) {
		t.Errorf("password %q must contain upper/lower/digit/symbol (got %v/%v/%v/%v)",
			a, hasUpper, hasLower, hasDigit, hasSymbol)
	}
}

// TestGenerateRandomPasswordMinLength verifies a requested length below the
// number of classes is raised so every class is still represented.
func TestGenerateRandomPasswordMinLength(t *testing.T) {
	pw, err := generateRandomPassword(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 4 {
		t.Fatalf("len=%d, want 4 (one per class)", len(pw))
	}
}

// TestEnsureGeneratedPassword verifies the driver generates, stores, and
// wires the random password into the clone spec, and is a no-op when off.
func TestEnsureGeneratedPassword(t *testing.T) {
	dir := t.TempDir()
	d := NewDriver("n", dir)
	d.VMName = "n"
	d.SSHPassword = "should-be-ignored"
	d.GeneratePassword = true

	if err := d.ensureGeneratedPassword(); err != nil {
		t.Fatal(err)
	}
	if !d.SetVMPassword {
		t.Error("SetVMPassword should be forced true when generating a password")
	}
	if d.SSHPassword == "" || d.SSHPassword == "should-be-ignored" {
		t.Fatalf("expected a generated password, got %q", d.SSHPassword)
	}

	stored, err := os.ReadFile(d.ResolveStorePath(generatedPasswordFile))
	if err != nil {
		t.Fatalf("generated password file: %v", err)
	}
	if strings.TrimSpace(string(stored)) != d.SSHPassword {
		t.Errorf("stored password %q != driver password %q", strings.TrimSpace(string(stored)), d.SSHPassword)
	}

	spec := d.buildCloneSpec()
	if spec.VMCustomization.Password != d.SSHPassword {
		t.Errorf("clone spec password %q != generated %q", spec.VMCustomization.Password, d.SSHPassword)
	}
	if !spec.VMCustomization.IsUpdateVMPassword || !spec.VMCustomization.UpdatePassword {
		t.Error("clone spec must request the password reset when generating")
	}

	off := NewDriver("n2", dir)
	off.SSHPassword = "keep-me"
	if err := off.ensureGeneratedPassword(); err != nil {
		t.Fatal(err)
	}
	if off.SSHPassword != "keep-me" || off.SetVMPassword {
		t.Fatalf("ensureGeneratedPassword must be a no-op when disabled (pw=%q set=%v)", off.SSHPassword, off.SetVMPassword)
	}

	// Safety net: setVmPassword with no password supplied must generate one
	// rather than send a blank password to FC.
	auto := NewDriver("n3", dir)
	auto.VMName = "n3"
	auto.SetVMPassword = true
	if err := auto.ensureGeneratedPassword(); err != nil {
		t.Fatal(err)
	}
	if auto.SSHPassword == "" || !auto.GeneratePassword {
		t.Fatalf("setVmPassword with an empty password should auto-generate (pw=%q gen=%v)", auto.SSHPassword, auto.GeneratePassword)
	}

	// setVmPassword with an operator-supplied password and no generate flag
	// must keep that password.
	manual := NewDriver("n4", dir)
	manual.SetVMPassword = true
	manual.SSHPassword = "operator-picked"
	if err := manual.ensureGeneratedPassword(); err != nil {
		t.Fatal(err)
	}
	if manual.SSHPassword != "operator-picked" || manual.GeneratePassword {
		t.Fatalf("operator password must be kept (pw=%q gen=%v)", manual.SSHPassword, manual.GeneratePassword)
	}
}

// TestAlignResetSSHUser verifies the SSH user is forced to root only when a
// password reset is requested (FC's Linux customization targets root and
// changePassword is not permitted for other users).
func TestAlignResetSSHUser(t *testing.T) {
	reset := NewDriver("n", "/tmp")
	reset.SSHUser = "ubuntu"
	reset.SetVMPassword = true
	reset.alignResetSSHUser()
	if reset.GetSSHUsername() != "root" {
		t.Fatalf("reset on: ssh user = %q, want root", reset.GetSSHUsername())
	}
	if reset.BaseDriver.SSHUser != "root" {
		t.Fatalf("reset on: BaseDriver.SSHUser = %q, want root", reset.BaseDriver.SSHUser)
	}

	noReset := NewDriver("n", "/tmp")
	noReset.SSHUser = "ubuntu"
	noReset.alignResetSSHUser()
	if noReset.GetSSHUsername() != "ubuntu" {
		t.Fatalf("reset off: ssh user = %q, want ubuntu (unchanged)", noReset.GetSSHUsername())
	}

	empty := NewDriver("n", "/tmp")
	empty.SSHUser = ""
	empty.SetVMPassword = true
	empty.alignResetSSHUser()
	if empty.GetSSHUsername() != "root" {
		t.Fatalf("reset on, empty user: ssh user = %q, want root", empty.GetSSHUsername())
	}
}
