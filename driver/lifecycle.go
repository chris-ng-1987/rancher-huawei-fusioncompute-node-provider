package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Random guest password generation for --fusioncompute-generate-password.
//
// FusionCompute's clone vmCustomization.password requires a concrete value;
// it does not generate one. When the operator asks the driver to generate a
// password we pick it here, send it as vmCustomization.password with
// isUpdateVmPassword=true (so FC resets the guest OS password on first boot),
// and persist it under the machine's store directory so it can be retrieved
// after provisioning. The value is never written to the logs.
const (
	generatedPasswordLength = 16
	generatedPasswordFile   = "fusioncompute-generated-password"

	// Character classes. Ambiguous glyphs (0/O, 1/l/I) and shell-hostile
	// characters (quotes, backslash, backtick, $, ;, &, |, <, >, parens,
	// whitespace) are excluded. Every class is represented at least once so
	// the result passes FusionCompute's Linux password complexity rules.
	passwordUpperChars  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	passwordLowerChars  = "abcdefghijkmnopqrstuvwxyz"
	passwordDigitChars  = "23456789"
	passwordSymbolChars = "!@#%^*-_=+"
	passwordAllChars    = passwordUpperChars + passwordLowerChars + passwordDigitChars + passwordSymbolChars
)

// randInt returns a cryptographically random int in [0, n).
func randInt(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("randInt: n must be positive, got %d", n)
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}

// generateRandomPassword returns a password of the requested length that
// contains at least one character from each class. It uses crypto/rand and a
// crypto/rand Fisher-Yates shuffle for the guaranteed classes.
func generateRandomPassword(length int) (string, error) {
	classes := []string{passwordUpperChars, passwordLowerChars, passwordDigitChars, passwordSymbolChars}
	if length < len(classes) {
		length = len(classes)
	}

	out := make([]byte, 0, length)
	for _, class := range classes {
		i, err := randInt(len(class))
		if err != nil {
			return "", err
		}
		out = append(out, class[i])
	}
	for len(out) < length {
		i, err := randInt(len(passwordAllChars))
		if err != nil {
			return "", err
		}
		out = append(out, passwordAllChars[i])
	}

	for i := len(out) - 1; i > 0; i-- {
		j, err := randInt(i + 1)
		if err != nil {
			return "", err
		}
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

// ensureGeneratedPassword creates a random guest password when
// --fusioncompute-generate-password is set, or when
// --fusioncompute-set-vm-password is set with no password supplied. It forces
// the FusionCompute password-reset flags on. The generated value is stored
// (mode 0600) in the machine's store directory as
// "fusioncompute-generated-password" and is also kept on the driver so it
// persists in the machine's config state.
func (d *Driver) ensureGeneratedPassword() error {
	// Explicit flag, or the safety net: resetting the password without one
	// supplied would otherwise send an empty/blank password to FC.
	generate := d.GeneratePassword || (d.SetVMPassword && d.SSHPassword == "")
	if !generate {
		return nil
	}

	if d.GeneratePassword && d.SSHPassword != "" {
		log.Infof("FusionCompute: --fusioncompute-generate-password set; ignoring supplied --fusioncompute-ssh-password")
	}

	pw, err := generateRandomPassword(generatedPasswordLength)
	if err != nil {
		return fmt.Errorf("fusioncompute: generate random password: %w", err)
	}
	d.SSHPassword = pw
	d.SetVMPassword = true
	d.GeneratePassword = true

	path := generatedPasswordFile
	if d.BaseDriver != nil {
		path = d.ResolveStorePath(generatedPasswordFile)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("fusioncompute: create directory for generated password %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(pw+"\n"), 0600); err != nil {
		return fmt.Errorf("fusioncompute: write generated password to %s: %w", path, err)
	}
	log.Infof("FusionCompute: generated a random guest password for %s (stored at %s)", d.VMName, path)
	return nil
}

// alignResetSSHUser forces the SSH user to root when the clone resets the
// guest password. FusionCompute's Linux customization always targets root
// (vmCustomization carries no username, and the changePassword action is not
// permitted for non-root users), so logging in as any other user with the new
// password would fail.
func (d *Driver) alignResetSSHUser() {
	if !d.SetVMPassword {
		return
	}
	current := d.GetSSHUsername()
	if current == "root" {
		return
	}
	log.Infof("FusionCompute: password reset targets root; overriding SSH user %q -> %q", current, "root")
	d.SSHUser = "root"
	if d.BaseDriver != nil {
		d.BaseDriver.SSHUser = "root"
	}
}

// Create is the heart of the driver. It performs the same workflow as
// the Ansible playbook the reference provisioning workflow:
//
//  1. authenticate against /service/session
//  2. resolve the site URI
//  3. POST /vms/{template}/action/clone with cpu/memory/disk/NIC/IP
//  4. poll the returned task URI until progress==100 & status==success
//  5. capture the VM URN so later operations can find the host
//
// On success, GetIP() returns the guest IP discovered from FusionCompute and
// docker-machine can SSH in using the SSH key supplied via
// --fusioncompute-ssh-key-path
// (or the standard docker-machine --ssh-key-path / MACHINE_SSH_KEY_PATH).
// Password auth is not supported by libmachine v0.16.x; if the template
// does not contain a public key, pass --fusioncompute-ssh-password with
// --fusioncompute-set-vm-password so the VM's root password is set on
// first boot, then log in out-of-band to install the key.
func (d *Driver) Create() (err error) {
	if d.VMName == "" {
		d.VMName = d.BaseDriver.MachineName
	}
	log.Infof("FusionCompute: provisioning %q from template %q on %s", d.VMName, d.TemplateID, d.DCSHostname)

	if err := d.ensureSSHKey(); err != nil {
		return err
	}

	if err := d.ensureGeneratedPassword(); err != nil {
		return err
	}

	// FC's Linux guest customization resets the root account's password (the
	// clone API has no username field, and changePassword is not permitted for
	// non-root users). Keep the SSH user in sync so the key-install step — and
	// docker-machine's later logins — use the account whose password was set.
	d.alignResetSSHUser()

	c, err := d.newClient()
	if err != nil {
		return err
	}

	spec := d.buildCloneSpec()

	log.Debugf("FusionCompute: POST clone request")
	task, err := c.CloneVM(d.TemplateID, spec)
	if err != nil {
		return fmt.Errorf("fusioncompute: clone request failed: %w", err)
	}
	log.Debugf("fusioncompute: clone spec was: name=%q description=%q", spec.Name, spec.Description)
	// FC's clone response carries two URIs:
	//   - task.URI      → new VM URN  (e.g. /service/sites/.../vms/i-000001B0)
	//   - task.TaskURI  → task tracker to poll for completion
	// The Ansible playbook reads these as `json.uri` and `json.taskUri`
	// respectively (the reference provisioning workflow).
	log.Infof("FusionCompute: clone task submitted: %s (vm=%s)", task.TaskURI, task.URI)

	// Poll the task URI until completion. We use task.TaskURI (NOT
	// task.URI, which is the VM URN and would never return progress).
	finished, err := c.WaitTask(task.TaskURI, 60, 15*time.Second)
	if err != nil {
		return fmt.Errorf("fusioncompute: clone task did not complete: %w", err)
	}
	if finished.Status != "success" {
		return fmt.Errorf("fusioncompute: clone task failed: %s (progress=%d)", finished.Status, finished.Progress)
	}

	// Prefer the VM URN returned alongside the task URI in the clone
	// response; fall back to a name-based lookup if FC didn't include
	// it (older builds).
	d.VMMoRef = task.URI
	if d.VMMoRef == "" {
		log.Debugf("FusionCompute: clone response did not include VM URI; querying by name")
		vm, err := c.findVMByName(d.VMName)
		if err != nil {
			return fmt.Errorf("fusioncompute: could not resolve new VM URN: %w", err)
		}
		d.VMMoRef = vm.URI
	}

	// The VM now exists. If any later step fails, delete it so a failed
	// create does not leave an orphan VM in FusionCompute. Opt out with
	// --fusioncompute-keep-failed-vm=true when you need to debug.
	if !d.KeepFailedVM {
		defer func() {
			if err != nil {
				log.Warnf("FusionCompute: provisioning failed (%v); deleting VM %s to avoid an orphan (set --fusioncompute-keep-failed-vm=true to keep it)", err, d.VMMoRef)
				if rmErr := d.Remove(); rmErr != nil {
					log.Warnf("FusionCompute: cleanup of VM %s failed: %v", d.VMMoRef, rmErr)
				}
			}
		}()
	}

	// Resolve the guest IP docker-machine will SSH to. The clone task does
	// not report the address the guest picks up at boot, so read it from the
	// VM's spec (GET {vmURI}) and poll until the guest has one.
	ip, err := d.waitForGuestIP(c, 40, 15*time.Second)
	if err != nil {
		return err
	}
	d.IPAddr = ip
	log.Infof("FusionCompute: VM %s provisioned at %s", d.VMMoRef, d.IPAddr)
	if err := d.waitForSSH(5*time.Minute, 5*time.Second); err != nil {
		return err
	}
	return d.installAuthorizedKey(c)
}

func (d *Driver) ensureSSHKey() error {
	path := d.GetSSHKeyPath()
	if path == "" && d.BaseDriver != nil {
		path = d.BaseDriver.GetSSHKeyPath()
		d.SSHKeyPath = path
		d.BaseDriver.SSHKeyPath = path
	}
	if path == "" {
		return fmt.Errorf("fusioncompute: no SSH key path")
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	log.Infof("Generating SSH keypair at %s", path)
	return ssh.GenerateSSHKey(path)
}

// waitForGuestIP polls the cloned VM's spec (GET {vmURI}) until
// FusionCompute reports a guest IP. The clone/provision task only tracks the
// clone itself and does not reflect the address the guest obtains on boot, so
// the IP is read from vmConfig.nics[0].ip (see VM.GuestIP).
func (d *Driver) waitForGuestIP(c *fcClient, attempts int, interval time.Duration) (string, error) {
	var last error
	for i := 0; i < attempts; i++ {
		vm, err := c.GetVM(d.VMMoRef)
		if err != nil {
			last = err
		} else if ip := vm.GuestIP(); ip != "" {
			return ip, nil
		} else {
			last = fmt.Errorf("guest IP empty")
		}
		time.Sleep(interval)
	}
	return "", fmt.Errorf("fusioncompute: no guest IP after clone: %w", last)
}

func (d *Driver) waitForSSH(timeout, interval time.Duration) error {
	addr := d.sshAddr()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, interval)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		time.Sleep(interval)
	}
	return fmt.Errorf("fusioncompute: SSH %s not reachable: %w", addr, last)
}

// buildCloneSpec translates our driver fields into the JSON body the
// FusionCompute clone endpoint expects. The struct is defined in
// client.go and is deliberately close to the playbook's server_spec.
//
// We intentionally populate every field the playbook sets, including
// those whose value is the empty string / null, so the JSON shape
// matches what FC's schema validator accepts.
//
// VMName fall-back: docker-machine only guarantees that
// BaseDriver.MachineName is populated (that's the positional argument
// to `docker-machine create <name>`). Our optional
// `fusioncompute-vm-description` flag does NOT cover VMName, and
// `NewDriver` is not always invoked on the plugin server's path.
// Fall back to MachineName when VMName is empty so the cloned VM
// always has a non-empty name (FC rejects "" with 10000001).
func (d *Driver) buildCloneSpec() *CloneSpec {
	vmName := d.VMName
	if vmName == "" {
		vmName = d.BaseDriver.MachineName
	}
	spec := &CloneSpec{
		Name:          vmName,
		Description:   d.VMDescription,
		Group:         "",
		IsBindingHost: "false",
		Location:      d.Location,
		AutoBoot:      true,
	}
	// Group is always set to "" to match the playbook's `group: ""`.
	// FC's schema validator accepts this on the build our playbook
	// was authored against (the only known good body).

	// Properties: the playbook always sends three explicit null
	// sub-fields (bootOptions, isEnableHa, vmFaultProcess). The FC
	// schema validator expects `properties` to be present with all
	// three nulls; omitting it was the previous errorCode 10000001
	// cause. We allocate the struct with all-nil interface{} fields
	// so it marshals to {"bootOptions":null,"isEnableHa":null,"vmFaultProcess":null}.
	spec.VMConfig.Properties.BootOptions = nil
	spec.VMConfig.Properties.IsEnableHA = nil
	spec.VMConfig.Properties.VMFaultProcess = nil

	spec.VMConfig.CPU.Quantity = strconv.Itoa(d.CPUCount)
	spec.VMConfig.CPU.CoresPerSocket = 1
	spec.VMConfig.CPU.Reservation = 0
	spec.VMConfig.CPU.Weight = "1000"
	spec.VMConfig.CPU.Limit = 0
	spec.VMConfig.CPU.CPUHotPlug = 1
	spec.VMConfig.CPU.CPUPolicy = "shared"
	spec.VMConfig.CPU.CPUThreadPolicy = "prefer"

	memMB := d.MemoryMB
	spec.VMConfig.Memory.QuantityMB = strconv.Itoa(memMB)
	spec.VMConfig.Memory.Reservation = 0
	spec.VMConfig.Memory.Weight = strconv.Itoa(memMB * 10)
	spec.VMConfig.Memory.HugePage = "4K"
	spec.VMConfig.Memory.Limit = 0
	spec.VMConfig.Memory.MemHotPlug = 1

	// Single boot disk — same shape as the playbook's `disks:` block.
	disk := struct {
		DatastoreURN    string `json:"datastoreUrn"`
		QuantityGB      int    `json:"quantityGB"`
		SequenceNum     int    `json:"sequenceNum"`
		Type            string `json:"type"`
		IndepDisk       bool   `json:"indepDisk"`
		PersistentDisk  bool   `json:"persistentDisk"`
		IsThin          bool   `json:"isThin"`
		VolType         int    `json:"volType"`
		PCIType         string `json:"pciType"`
		BootOrder       int    `json:"bootOrder"`
		Encrypted       string `json:"encrypted"`
		StorageProtocol string `json:"storageProtocol"`
	}{
		DatastoreURN:    d.DatastoreURN,
		QuantityGB:      d.DiskGB,
		SequenceNum:     1,
		Type:            "normal",
		IndepDisk:       false,
		PersistentDisk:  true,
		IsThin:          true,
		VolType:         0,
		PCIType:         "VIRTIO",
		BootOrder:       1,
		Encrypted:       "0",
		StorageProtocol: "0",
	}
	spec.VMConfig.Disks = append(spec.VMConfig.Disks, disk)

	nic := struct {
		Name             string `json:"name"`
		PortGroupURN     string `json:"portGroupUrn"`
		SequenceNum      int    `json:"sequenceNum"`
		VirtIO           int    `json:"virtIo"`
		BootOrder        int    `json:"bootOrder"`
		ConnectAtPowerOn bool   `json:"connectAtPowerOn"`
	}{
		Name:             "Network Adapter 0",
		PortGroupURN:     d.PortGroupURN,
		SequenceNum:      0,
		VirtIO:           1,
		BootOrder:        3,
		ConnectAtPowerOn: true,
	}
	spec.VMConfig.NICs = append(spec.VMConfig.NICs, nic)

	spec.VMCustomization.OSType = "Linux"
	spec.VMCustomization.UpdatePassword = d.SetVMPassword
	// vmcustomizationId: playbook sends null. We mirror that exactly;
	// an empty string here is rejected by FC with errorCode 10000001.
	spec.VMCustomization.VMCustomizationID = nil
	spec.VMCustomization.Hostname = vmName // same fall-back as spec.Name

	// Password handling:
	//   - By default (matches the Ansible playbook), password="" and
	//     isUpdateVmPassword=false so FC keeps the template's root
	//     password and does not reset it on first boot.
	//   - When the user supplies --fusioncompute-ssh-password together
	//     with --fusioncompute-set-vm-password, we send the password
	//     and flip isUpdateVmPassword=true so FC actually resets the
	//     cloned VM's root account on first boot. This is a recovery
	//     path for templates that don't have a baked-in public key —
	//     the operator can SSH in with the password, install the key,
	//     and then docker-machine can use key-based auth.
	// FC's schema validator requires that isUpdateVmPassword and the
	// password value be sent as the matching pair; we therefore always
	// send both fields and only the values change.
	spec.VMCustomization.Password = d.SSHPassword
	spec.VMCustomization.IsUpdateVMPassword = d.SetVMPassword

	// NIC addressing: DHCP. Send a bare `{ipVersion,sequenceNum}` entry so
	// the guest obtains its address at boot. The slice must be non-empty:
	// FC rejects an empty/absent nicSpecification on this build.
	spec.VMCustomization.NICSpecification = []NICSpecification{{
		IPVersion:   4,
		SequenceNum: 1,
	}}

	return spec
}

// findVMByName is a small helper for the post-clone name -> URN lookup.
func (c *fcClient) findVMByName(name string) (*VM, error) {
	if err := c.ensureSite(); err != nil {
		return nil, err
	}
	// Many FusionCompute versions support a filter; fall back to listing.
	var resp struct {
		Vms []VM `json:"vms"`
	}
	if err := c.doJSON(http.MethodGet, c.siteURI+"/vms?filter=name eq "+name, nil, &resp); err != nil {
		return nil, err
	}
	for i := range resp.Vms {
		if resp.Vms[i].Name == name {
			return &resp.Vms[i], nil
		}
	}
	return nil, fmt.Errorf("no VM named %q in site %s", name, c.siteURI)
}

// pollURI picks the right URI to poll from a Task response. FC's
// clone endpoint returns both `uri` (the new VM URN) and `taskUri`
// (the task tracker); action endpoints (start/stop/delete) return
// only `uri`, which IS the task tracker on those endpoints. This
// helper picks `taskUri` if present, falling back to `uri`.
func pollURI(t *Task) string {
	if t.TaskURI != "" {
		return t.TaskURI
	}
	return t.URI
}

// Remove stops (if running) and deletes the VM.
func (d *Driver) Remove() error {
	c, err := d.newClient()
	if err != nil {
		return err
	}
	if d.VMMoRef == "" {
		name := d.VMName
		if name == "" && d.BaseDriver != nil {
			name = d.BaseDriver.MachineName
		}
		if name != "" {
			if vm, err := c.findVMByName(name); err == nil {
				d.VMMoRef = vm.URI
			}
		}
	}
	if d.VMMoRef == "" {
		return nil
	}

	if t, err := c.PowerAction(d.VMMoRef, "forceStop"); err == nil {
		_, _ = c.WaitTask(pollURI(t), 12, 5*time.Second)
	}

	if err := c.DeleteVM(d.VMMoRef); err != nil {
		if strings.Contains(err.Error(), "status=404") {
			d.VMMoRef = ""
			return nil
		}
		return fmt.Errorf("fusioncompute: delete failed: %w", err)
	}
	d.VMMoRef = ""
	return nil
}

// Start powers the VM on.
func (d *Driver) Start() error {
	c, err := d.newClient()
	if err != nil {
		return err
	}
	t, err := c.PowerAction(d.VMMoRef, "start")
	if err != nil {
		return err
	}
	_, err = c.WaitTask(pollURI(t), 30, 5*time.Second)
	return err
}

// Stop performs a graceful guest shutdown.
func (d *Driver) Stop() error {
	c, err := d.newClient()
	if err != nil {
		return err
	}
	t, err := c.PowerAction(d.VMMoRef, "stop")
	if err != nil {
		return err
	}
	_, err = c.WaitTask(pollURI(t), 30, 5*time.Second)
	return err
}

// Restart combines Stop + Start. Some FusionCompute versions also expose
// a native "reboot" action; we use the explicit pair for portability.
func (d *Driver) Restart() error {
	if err := d.Stop(); err != nil {
		return err
	}
	return d.Start()
}

// Kill is an unconditional hard power-off.
func (d *Driver) Kill() error {
	c, err := d.newClient()
	if err != nil {
		return err
	}
	t, err := c.PowerAction(d.VMMoRef, "forceStop")
	if err != nil {
		return err
	}
	_, err = c.WaitTask(pollURI(t), 30, 5*time.Second)
	return err
}

// GetURL is unused for IaaS drivers but docker-machine's interface requires it.
func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, "2376")), nil
}

// GetSSHKeyPath returns the SSH private key path. The driver reads
// this from --fusioncompute-ssh-key-path (env FUSIONCOMPUTE_SSH_KEY_PATH)
// in SetConfigFromFlags; when unset it returns "" and libmachine falls
// back to its standard --ssh-key-path / MACHINE_SSH_KEY_PATH lookup.
//
// Note: the standard docker-machine --ssh-key-path flag is *not* a
// built-in global flag in v0.16.x (libmachine/drivers/rpc/server.go
// only forwards flags declared by GetCreateFlags()), so users must
// pass --fusioncompute-ssh-key-path or set FUSIONCOMPUTE_SSH_KEY_PATH.
func (d *Driver) GetSSHKeyPath() string {
	if d.SSHKeyPath != "" {
		return d.SSHKeyPath
	}
	if d.BaseDriver != nil {
		return d.BaseDriver.GetSSHKeyPath()
	}
	return ""
}

func (d *Driver) publicSSHKeyPath() string {
	return d.GetSSHKeyPath() + ".pub"
}

func (d *Driver) sshAddr() string {
	port := d.SSHPort
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(d.IPAddr, strconv.Itoa(port))
}

// keyInstallTimeout bounds how long we wait for the guest's SSH password auth
// to accept the reset password. FusionCompute customization can lag sshd
// coming up (and the guest IP can change), so this is generous.
const keyInstallTimeout = 10 * time.Minute

func (d *Driver) installAuthorizedKey(c *fcClient) error {
	if d.SSHPassword == "" {
		log.Infof("No guest password set; template must already trust %s", d.publicSSHKeyPath())
		return nil
	}
	pub, err := os.ReadFile(d.publicSSHKeyPath())
	if err != nil {
		return fmt.Errorf("fusioncompute: read public key: %w", err)
	}
	key := strings.TrimSpace(string(pub))
	user := d.GetSSHUsername()
	log.Infof("FusionCompute: installing SSH key for user %q via password login (password length %d) at %s", user, len(d.SSHPassword), d.sshAddr())
	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.Password(d.SSHPassword)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         20 * time.Second,
	}

	deadline := time.Now().Add(keyInstallTimeout)
	var last error
	attempt := 0
	lastLog := time.Time{}
	for time.Now().Before(deadline) {
		attempt++

		// FC can report an address before the guest has settled, and the
		// DHCP lease can change while customization runs. Re-resolve on
		// every attempt so we follow the VM's current address instead of
		// pinning a stale one for the whole window.
		d.refreshGuestIP(c)

		client, err := gossh.Dial("tcp", d.sshAddr(), cfg)
		if err != nil {
			last = err
			if attempt == 1 || time.Since(lastLog) >= 60*time.Second {
				log.Infof("FusionCompute: waiting for password SSH on %s for %q (attempt %d): %v", d.sshAddr(), user, attempt, err)
				lastLog = time.Now()
			}
			time.Sleep(5 * time.Second)
			continue
		}
		session, err := client.NewSession()
		if err != nil {
			_ = client.Close()
			return err
		}
		cmd := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && umask 077 && grep -qxF " + strconv.Quote(key) + " ~/.ssh/authorized_keys 2>/dev/null || echo " + strconv.Quote(key) + " >> ~/.ssh/authorized_keys"
		err = session.Run(cmd)
		_ = session.Close()
		_ = client.Close()
		if err != nil {
			return fmt.Errorf("fusioncompute: install authorized_keys: %w", err)
		}
		log.Infof("Installed SSH public key for user %s", user)
		return nil
	}
	return fmt.Errorf("fusioncompute: password SSH for key install failed for user %q at %s: %w (verify the guest password; if it is valid, ensure --fusioncompute-ssh-user matches the account FusionCompute resets, normally root)", user, d.sshAddr(), last)
}

// refreshGuestIP re-reads the VM from FusionCompute and updates d.IPAddr when
// FC reports a different, usable guest address. Best-effort: errors leave the
// current address untouched.
func (d *Driver) refreshGuestIP(c *fcClient) {
	if c == nil || d.VMMoRef == "" {
		return
	}
	vm, err := c.GetVM(d.VMMoRef)
	if err != nil || vm == nil {
		return
	}
	ip := vm.GuestIP()
	if ip == "" || ip == d.IPAddr {
		return
	}
	log.Infof("FusionCompute: guest IP for %s changed %s -> %s", d.VMName, d.IPAddr, ip)
	d.IPAddr = ip
}

// drivers.Driver interface compliance check at compile time.
var _ drivers.Driver = (*Driver)(nil)
