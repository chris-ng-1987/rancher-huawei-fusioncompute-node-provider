package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/mcnflag"
	"github.com/rancher/machine/libmachine/state"
)

// Driver implements the docker-machine Driver interface for Huawei
// FusionCompute (a.k.a. DCS). It provisions a Linux VM from a template
// using the FusionCompute REST API and returns the configured IP so
// docker-machine can SSH into the new host and install Docker.
//
// The set of driver flags mirrors the inputs used in the reference
// Ansible playbook the reference provisioning workflow, translated from
// `vars:` into `mcnflag.Flag` entries that docker-machine exposes on
// the command line via `-d fusioncompute --<flag> <value>`.
type Driver struct {
	*drivers.BaseDriver

	// --- FusionCompute connection ---
	DCSHostname   string // e.g. "dcs.example.com"  -> https://<host>/service/...
	DCSPort       int    // TCP port for the API; defaults to 7443 if DCSHostname has no :port
	DCSUsername   string // X-Auth-User
	DCSPassword   string // X-Auth-Key
	DCSSiteURI    string // required site URI, e.g. "/service/sites/1"
	SkipTLSVerify bool   // skip VRM certificate verification; only for self-signed certificates

	// --- Provisioning inputs ---
	TemplateID    string // template (or VM) ID to clone from, e.g. "i-000001BC"
	CPUCount      int    // num CPUs
	MemoryMB      int    // memory in MB
	DiskGB        int    // system disk size
	PortGroupURN  string // network port group URN
	DatastoreURN  string // destination datastore URN
	VMName        string // hostname of the new VM
	VMDescription string // optional description (audit fields)
	Location      string // FusionCompute location URN

	// Guest password reset is exposed via --fusioncompute-set-vm-password.
	// FusionCompute only honours vmCustomization.password when
	// isUpdateVmPassword=true is also set; with the flag off (the default,
	// mirroring the reference Ansible playbook) the cloned VM keeps
	// whatever password was baked into the template.

	// --- SSH (used by docker-machine to log into the new host) ---
	SSHUser          string // OS user docker-machine will log in as
	SSHPort          int    // SSH port on the VM
	SSHKeyPath       string // private key path (key-based auth only)
	SSHPrivateKey    string `json:"-"` // unencrypted key contents; never persisted in machine state
	SSHPassword      string // optional: sent via vmCustomization.password
	SetVMPassword    bool   // when true, set isUpdateVmPassword=true on clone
	GeneratePassword bool   // when true, generate a random guest password and reset it on clone
	KeepFailedVM     bool   // when true, do not delete the VM if Create() fails after the clone
	// SSH auth is **key-based only** because libmachine v0.16.x
	// hard-codes `-o PasswordAuthentication=no` and the
	// StandardSSHClientCreator never adds passwords to the ssh.ClientConfig.
	//
	// There are three ways to provide the key docker-machine will use:
	//
	//  1. --fusioncompute-ssh-private-key: the operator supplies an
	//     unencrypted private key. The driver writes it to the machine
	//     store and uses it for the initial login, so the template must
	//     already trust the matching public key. No password is sent to
	//     FusionCompute. Mutually exclusive with options 2 and 3.
	//  2. --fusioncompute-ssh-key-path: path to a pre-provisioned private
	//     key on the host running docker-machine/the Rancher driver. The
	//     template must already trust the matching public key.
	//  3. neither: the driver generates a key pair at <machine>/id_rsa and
	//     --fusioncompute-ssh-password (plus --fusioncompute-set-vm-password)
	//     is used once to install the public key on the guest.
	//
	// --fusioncompute-ssh-password is only a bootstrap/fallback credential;
	// libmachine itself never uses it for SSH.

	// --- Runtime state set during Create() ---
	VMMoRef string // VM URN, set after clone succeeds
	IPAddr  string // observed guest IP, read from the VM spec after boot
}

// DefaultSSHPort is the only SSH port the driver supports. FusionCompute
// templates provisioned by this driver always run sshd on 22, so the port
// is fixed rather than user-configurable (there is no
// --fusioncompute-ssh-port flag).
const DefaultSSHPort = 22

// defaultVMDescription is shown on the cloned VM unless the operator supplies
// --fusioncompute-vm-description.
const defaultVMDescription = "Provisioned by docker-machine-driver-fusioncompute"

// normalizeSiteURI converts an operator-supplied site path to the canonical
// /service/sites/<id> form. Invalid values are returned empty so validate()
// can reject them.
func normalizeSiteURI(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/service/sites/") {
		id := strings.TrimPrefix(s, "/service/sites/")
		if id == "" || strings.Contains(id, "/") {
			return ""
		}
		return s
	}
	s = strings.TrimLeft(s, "/")
	s = strings.TrimPrefix(s, "service/sites/")
	s = strings.TrimPrefix(s, "sites/")
	if s == "" || strings.Contains(s, "/") {
		return ""
	}
	return "/service/sites/" + s
}

// NewDriver returns a fresh Driver with sensible defaults. It is
// registered with docker-machine via plugin.RegisterDriver in main.go.
//
// The `caCert`/`privateKey` arguments come from libmachine's plugin
// bootstrap: when --fusioncompute-ssh-key-path is supplied on the
// command line, libmachine passes the resolved key path through
// `privateKey` here and we record it on the driver so libmachine's
// SSH client can find it. Without --fusioncompute-ssh-key-path, the
// arguments are empty strings and SSHKeyPath stays "".
func NewDriver(hostName string, storePath string) *Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			MachineName: hostName,
			StorePath:   storePath,
			SSHUser:     "root",
			SSHPort:     DefaultSSHPort,
		},
		VMName:        hostName,
		VMDescription: defaultVMDescription,
		CPUCount:      2,
		MemoryMB:      4096,
		DiskGB:        250,
		SSHUser:       "root",
		SSHPort:       DefaultSSHPort,
	}
}

// GetCreateFlags registers the CLI flags exposed by `docker-machine create`.
// Each flag maps to a Driver field populated at create time.
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		// FusionCompute API connection
		mcnflag.StringFlag{
			Name:   "fusioncompute-dcs-hostname",
			Usage:  "FusionCompute host (FQDN or IP) used as the API base URL",
			EnvVar: "FUSIONCOMPUTE_DCS_HOSTNAME",
		},
		mcnflag.IntFlag{
			Name:   "fusioncompute-dcs-port",
			Usage:  "TCP port for the FusionCompute API (ignored if --fusioncompute-dcs-hostname already includes :port)",
			EnvVar: "FUSIONCOMPUTE_DCS_PORT",
			Value:  7443,
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-dcs-username",
			Usage:  "FusionCompute username (X-Auth-User)",
			EnvVar: "FUSIONCOMPUTE_DCS_USERNAME",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-dcs-password",
			Usage:  "FusionCompute password (X-Auth-Key)",
			EnvVar: "FUSIONCOMPUTE_DCS_PASSWORD",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-dcs-site-uri",
			Usage:  "FusionCompute site URI, e.g. /service/sites/1 (required; no auto-discovery)",
			EnvVar: "FUSIONCOMPUTE_DCS_SITE_URI",
		},
		mcnflag.BoolFlag{
			Name:   "fusioncompute-skip-tls-verify",
			Usage:  "Skip TLS certificate verification for the FusionCompute API; only needed for self-signed VRM certificates (default false: verify the certificate)",
			EnvVar: "FUSIONCOMPUTE_SKIP_TLS_VERIFY",
		},

		// VM spec
		mcnflag.StringFlag{
			Name:   "fusioncompute-template-id",
			Usage:  "ID of the template (or source VM) to clone, e.g. i-000001BC",
			EnvVar: "FUSIONCOMPUTE_TEMPLATE_ID",
		},
		mcnflag.IntFlag{
			Name:   "fusioncompute-cpu",
			Usage:  "vCPU count",
			EnvVar: "FUSIONCOMPUTE_CPU",
			Value:  2,
		},
		mcnflag.IntFlag{
			Name:   "fusioncompute-memory-mb",
			Usage:  "Memory in MB",
			EnvVar: "FUSIONCOMPUTE_MEMORY_MB",
			Value:  4096,
		},
		mcnflag.IntFlag{
			Name:   "fusioncompute-disk-gb",
			Usage:  "System disk size in GB",
			EnvVar: "FUSIONCOMPUTE_DISK_GB",
			Value:  250,
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-port-group-urn",
			Usage:  "URN of the port group for the VM NIC",
			EnvVar: "FUSIONCOMPUTE_PORT_GROUP_URN",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-datastore-urn",
			Usage:  "URN of the destination datastore",
			EnvVar: "FUSIONCOMPUTE_DATASTORE_URN",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-location",
			Usage:  "FusionCompute location URN (optional)",
			EnvVar: "FUSIONCOMPUTE_LOCATION",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-vm-description",
			Usage:  "Description shown on the cloned VM (default: " + defaultVMDescription + ")",
			EnvVar: "FUSIONCOMPUTE_VM_DESCRIPTION",
			Value:  defaultVMDescription,
		},

		// SSH access — used by docker-machine *after* the VM is up to
		// install Docker. docker-machine v0.16.x only supports key-based
		// SSH (PasswordAuthentication=no is hard-coded in
		// libmachine/ssh/client.go), so the key options below are the
		// supported paths. --fusioncompute-ssh-password is provided only
		// as a way to push a password into the cloned VM via
		// FusionCompute's vmCustomization.password field, so an operator
		// can log in out-of-band and install the public key.
		mcnflag.StringFlag{
			Name:   "fusioncompute-ssh-user",
			Usage:  "OS user docker-machine will SSH in as after the VM is up",
			EnvVar: "FUSIONCOMPUTE_SSH_USER",
			Value:  "root",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-ssh-private-key",
			Usage:  "Unencrypted private SSH key contents used for initial access to the new VM; the template must already trust the matching public key. Mutually exclusive with the password/reset and key-path options",
			EnvVar: "FUSIONCOMPUTE_SSH_PRIVATE_KEY",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-ssh-key-path",
			Usage:  "Path to a pre-provisioned private SSH key used to authenticate docker-machine to the new VM; the template must already trust the matching public key",
			EnvVar: "FUSIONCOMPUTE_SSH_KEY_PATH",
		},
		mcnflag.StringFlag{
			Name:   "fusioncompute-ssh-password",
			Usage:  "Password to set on the new VM's root account via FusionCompute vmCustomization.password (requires --fusioncompute-set-vm-password). NOTE: libmachine itself cannot use this for SSH — it is only useful as a fallback credential.",
			EnvVar: "FUSIONCOMPUTE_SSH_PASSWORD",
		},
		mcnflag.BoolFlag{
			Name:   "fusioncompute-set-vm-password",
			Usage:  "When true, send isUpdateVmPassword=true so FusionCompute resets the cloned VM's password to --fusioncompute-ssh-password on first boot",
			EnvVar: "FUSIONCOMPUTE_SET_VM_PASSWORD",
		},
		mcnflag.BoolFlag{
			Name:   "fusioncompute-generate-password",
			Usage:  "When true, generate a random guest password (requires FusionCompute guest customization), reset it on clone, and store it at <machine>/fusioncompute-generated-password; overrides --fusioncompute-ssh-password",
			EnvVar: "FUSIONCOMPUTE_GENERATE_PASSWORD",
		},
		mcnflag.BoolFlag{
			Name:   "fusioncompute-keep-failed-vm",
			Usage:  "When true, keep the cloned VM if provisioning fails after the clone (default false: the driver deletes it so failed creates do not leave orphan VMs)",
			EnvVar: "FUSIONCOMPUTE_KEEP_FAILED_VM",
		},
	}
}

// SetConfigFromFlags pulls each flag value out of opts and assigns it
// onto the Driver struct. It is the inverse of GetCreateFlags.
func (d *Driver) SetConfigFromFlags(opts drivers.DriverOptions) error {
	d.DCSHostname = opts.String("fusioncompute-dcs-hostname")
	d.DCSPort = opts.Int("fusioncompute-dcs-port")
	d.DCSUsername = opts.String("fusioncompute-dcs-username")
	d.DCSPassword = opts.String("fusioncompute-dcs-password")
	d.DCSSiteURI = normalizeSiteURI(opts.String("fusioncompute-dcs-site-uri"))
	d.SkipTLSVerify = opts.Bool("fusioncompute-skip-tls-verify")

	d.TemplateID = opts.String("fusioncompute-template-id")
	d.CPUCount = opts.Int("fusioncompute-cpu")
	d.MemoryMB = opts.Int("fusioncompute-memory-mb")
	d.DiskGB = opts.Int("fusioncompute-disk-gb")
	d.PortGroupURN = opts.String("fusioncompute-port-group-urn")
	d.DatastoreURN = opts.String("fusioncompute-datastore-urn")
	d.Location = opts.String("fusioncompute-location")
	if description := opts.String("fusioncompute-vm-description"); description != "" {
		d.VMDescription = description
	}

	d.SSHUser = opts.String("fusioncompute-ssh-user")
	// SSH port is fixed at 22 — see DefaultSSHPort.
	d.SSHPort = DefaultSSHPort
	d.SSHKeyPath = opts.String("fusioncompute-ssh-key-path")
	d.SSHPrivateKey = strings.TrimSpace(opts.String("fusioncompute-ssh-private-key"))
	d.SSHPassword = opts.String("fusioncompute-ssh-password")
	d.SetVMPassword = opts.Bool("fusioncompute-set-vm-password")
	d.GeneratePassword = opts.Bool("fusioncompute-generate-password")
	d.KeepFailedVM = opts.Bool("fusioncompute-keep-failed-vm")
	d.SetSwarmConfigFromFlags(opts)

	// Also surface SSH port/user/key through the BaseDriver so
	// docker-machine can wire its own SSH client without re-reading our
	// flags. BaseDriver.SSHKeyPath is the field libmachine's
	// StandardSSHClientCreator reads when building the auth method.
	d.BaseDriver.SSHUser = d.SSHUser
	d.BaseDriver.SSHPort = d.SSHPort
	if d.SSHKeyPath != "" {
		d.BaseDriver.SSHKeyPath = d.SSHKeyPath
	}

	// VMName fall-back. The driver has no --fusioncompute-vm-name flag
	// because docker-machine already gives us the machine name via the
	// positional argument to `docker-machine create <name>`, which it
	// stores on BaseDriver.MachineName. Some plugin paths (older
	// docker-machine builds, third-party wrappers) do not invoke
	// NewDriver(), so VMName can come in empty — in that case we use
	// MachineName so the cloned VM has a non-empty name (FC rejects
	// empty name with 10000001).
	if d.VMName == "" {
		d.VMName = d.BaseDriver.MachineName
	}

	return d.validate()
}

func (d *Driver) UnmarshalJSON(data []byte) error {
	type targetDriver Driver
	target := targetDriver(*d)
	if err := json.Unmarshal(data, &target); err != nil {
		return fmt.Errorf("error unmarshalling fusioncompute driver config: %w", err)
	}
	*d = Driver(target)
	if d.BaseDriver == nil {
		d.BaseDriver = &drivers.BaseDriver{}
	}
	// SSH port is fixed: ignore any older saved config that carried a
	// different value.
	d.SSHPort = DefaultSSHPort
	return nil
}

// validate enforces the minimum set of required fields. It is called
// from SetConfigFromFlags and from Create as a safety net.
func (d *Driver) validate() error {
	missing := []string{}
	if d.DCSHostname == "" {
		missing = append(missing, "fusioncompute-dcs-hostname")
	}
	if d.DCSUsername == "" {
		missing = append(missing, "fusioncompute-dcs-username")
	}
	if d.DCSPassword == "" {
		missing = append(missing, "fusioncompute-dcs-password")
	}
	if d.DCSSiteURI == "" {
		missing = append(missing, "fusioncompute-dcs-site-uri")
	}
	if d.TemplateID == "" {
		missing = append(missing, "fusioncompute-template-id")
	}
	if d.PortGroupURN == "" {
		missing = append(missing, "fusioncompute-port-group-urn")
	}
	if d.DatastoreURN == "" {
		missing = append(missing, "fusioncompute-datastore-urn")
	}
	if len(missing) > 0 {
		return fmt.Errorf("fusioncompute driver: missing required flags: %v", missing)
	}

	siteID := strings.TrimPrefix(d.DCSSiteURI, "/service/sites/")
	if !strings.HasPrefix(d.DCSSiteURI, "/service/sites/") || siteID == "" || strings.Contains(siteID, "/") {
		return fmt.Errorf("fusioncompute driver: site URI (dcsSiteUri / --fusioncompute-dcs-site-uri) must look like /service/sites/<id>")
	}

	// SSH auth modes are mutually exclusive. A supplied private key is used
	// directly for the initial login; there is no password bootstrap, and a
	// separate key path would make it ambiguous which key docker-machine
	// should use.
	if d.SSHPrivateKey != "" {
		if d.SSHKeyPath != "" {
			return fmt.Errorf("fusioncompute driver: --fusioncompute-ssh-private-key and --fusioncompute-ssh-key-path are mutually exclusive")
		}
		if d.SSHPassword != "" || d.SetVMPassword || d.GeneratePassword {
			return fmt.Errorf("fusioncompute driver: --fusioncompute-ssh-private-key cannot be combined with --fusioncompute-ssh-password, --fusioncompute-set-vm-password, or --fusioncompute-generate-password")
		}
	}
	return nil
}

// DriverName returns the name docker-machine uses to refer to this driver.
func (d *Driver) DriverName() string { return DriverName }

// GetIP returns the IP that docker-machine should use to SSH into the host.
func (d *Driver) GetIP() (string, error) {
	if d.IPAddr == "" {
		return "", fmt.Errorf("fusioncompute: IP not yet known for machine %s", d.MachineName)
	}
	return d.IPAddr, nil
}

// GetSSHHostname returns the hostname to use for SSH. We use the IP since
// FusionCompute does not necessarily publish a DNS name for the new VM.
func (d *Driver) GetSSHHostname() (string, error) { return d.GetIP() }

func (d *Driver) GetSSHUsername() string {
	if d.SSHUser == "" {
		d.SSHUser = "root"
	}
	if d.BaseDriver != nil {
		d.BaseDriver.SSHUser = d.SSHUser
	}
	return d.SSHUser
}

func (d *Driver) GetMachineId() (string, error) {
	if d.VMMoRef == "" {
		return "", fmt.Errorf("fusioncompute: machine %s has no VMMoRef", d.MachineName)
	}
	return d.VMMoRef, nil
}

func (d *Driver) Upgrade() error {
	return fmt.Errorf("upgrade is not supported for fusioncompute driver")
}

// GetSSHPort returns the SSH port. It is fixed at 22 (DefaultSSHPort); the
// driver does not expose a way to override it.
func (d *Driver) GetSSHPort() (int, error) {
	return DefaultSSHPort, nil
}

// GetState reports the current VM state. docker-machine uses this to
// surface "Running"/"Stopped" etc.
func (d *Driver) GetState() (state.State, error) {
	c, err := d.newClient()
	if err != nil {
		return state.None, err
	}
	if d.VMMoRef == "" {
		return state.None, fmt.Errorf("fusioncompute: machine %s has no VMMoRef; was Create() run?", d.MachineName)
	}
	vm, err := c.GetVM(d.VMMoRef)
	if err != nil {
		return state.None, err
	}
	return d.mapState(vm.Status, vm.PowerState), nil
}

// PreCreateCheck is called by docker-machine before Create. It validates
// flags that don't need an API call and lets us fail fast with a clear
// message instead of waiting for the clone task to fail.
func (d *Driver) PreCreateCheck() error {
	return d.validate()
}

// helper to coerce "running"/"stopped" + power state into docker-machine state.State
func (d *Driver) mapState(vmStatus, powerState string) state.State {
	switch {
	case powerState == "running" || vmStatus == "running":
		return state.Running
	case powerState == "stopped" || vmStatus == "stopped":
		return state.Stopped
	case vmStatus == "creating" || vmStatus == "clone" || vmStatus == "starting":
		return state.Starting
	default:
		// Fall back to a generic "exists" so docker-machine still considers
		// the host known without crashing.
		return state.None
	}
}

// fmtPort is a small helper used by log messages.
func fmtPort(p int) string { return strconv.Itoa(p) }
