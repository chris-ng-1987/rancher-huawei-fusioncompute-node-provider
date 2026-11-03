package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rancher/machine/libmachine/log"
	"github.com/rancher/machine/libmachine/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const (
	// customScriptTimeout bounds the optional post-create script. The script
	// runs while Create() is still on the stack, so a hung script must not
	// block provisioning forever.
	customScriptTimeout = 10 * time.Minute

	// customScriptOutputLimit keeps a noisy script from filling the Rancher
	// logs (or an error message) with unbounded output.
	customScriptOutputLimit = 4 * 1024

	// Rancher's machine provision job mounts file-to-field-alias content
	// here; see nodedriver.cattle.io/file-to-field-aliases.
	rancherMachineFilesPath = "/path/to/machine/files"

	// rancherMachineBootstrapPath is where Rancher's machine provision job
	// mounts the generated node bootstrap script (the system-agent
	// installer) for this machine; see
	// pkg/controllers/capr/machineprovision/template.go in rancher/rancher.
	// The driver runs in the same container, so the file is readable.
	rancherMachineBootstrapPath = "/run/secrets/machine/value"

	// customScriptFilePrefix lets CLI/API callers pass a script file path
	// explicitly without depending on Rancher's mount path.
	customScriptFilePrefix = "file://"

	// maxCustomScriptBytes bounds both inline and file-backed scripts. Scripts
	// are sent through SSH stdin, so this is deliberately generous but finite.
	maxCustomScriptBytes = 1 << 20
)

// customScriptContents returns the shell script to run. A blank script means
// "do nothing".
//
// The value can be:
//   - inline shell text (the default for Rancher UI/CLI usage)
//   - a path under Rancher's machine-files mount, when the NodeDriver uses
//     nodedriver.cattle.io/file-to-field-aliases: customScript:customScript
//   - an explicit file:///path/to/script value
//
// The script is written to the remote bash process over SSH stdin, so no
// shell interpolation is involved; quotes, `$`, and newlines are preserved.
// It is not a place for secrets; the machine config still contains the field.
func (d *Driver) customScriptContents() ([]byte, error) {
	raw := d.CustomScript
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var (
		script []byte
		err    error
	)

	switch {
	case strings.HasPrefix(raw, customScriptFilePrefix):
		script, err = readCustomScriptFile(strings.TrimPrefix(raw, customScriptFilePrefix))
	case strings.HasPrefix(raw, rancherMachineFilesPath+"/"):
		script, err = readCustomScriptFile(raw)
	default:
		script = []byte(raw)
	}
	if err != nil {
		return nil, err
	}
	if len(script) > maxCustomScriptBytes {
		return nil, fmt.Errorf("fusioncompute: customScript is %d bytes, maximum is %d", len(script), maxCustomScriptBytes)
	}
	return script, nil
}

func readCustomScriptFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("fusioncompute: stat customScript file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("fusioncompute: customScript file %s is not a regular file", path)
	}
	if info.Size() > maxCustomScriptBytes {
		return nil, fmt.Errorf("fusioncompute: customScript file %s is %d bytes, maximum is %d", path, info.Size(), maxCustomScriptBytes)
	}
	script, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fusioncompute: read customScript file %s: %w", path, err)
	}
	return script, nil
}

// customScriptCommand returns the remote command used to execute the script.
// The driver normally logs in as root. For a non-root SSH user we require
// passwordless sudo; an interactive sudo prompt cannot be satisfied through
// the non-TTY session.
func (d *Driver) customScriptCommand() string {
	if d.GetSSHUsername() == "root" {
		return "bash -s"
	}
	return "sudo -n bash -s"
}

// rancherBootstrapMarkers identify Rancher's generated node bootstrap script
// and the cluster registration command. Both install and start the Rancher
// system agent (and, as a result, the RKE2/K3s node bootstrap), so putting
// either in customScript runs the node bootstrap twice.
var rancherBootstrapMarkers = []string{
	"system-agent-install.sh",
	"rancher-system-agent",
	"get.rke2.io",
	"get.k3s.io",
	rancherMachineBootstrapPath,
}

// isRancherNodeBootstrap reports whether script is Rancher's node
// bootstrap/registration script rather than user host preparation.
//
// Rancher runs its generated init script (--custom-install-script) over SSH
// after Create() returns. Executing the same or an equivalent script from
// customScript bootstraps the node twice: the first run initializes
// /var/lib/rancher/rke2 (or /var/lib/rancher/k3s) with one token and the
// second fails with "bootstrap data already found and encrypted with
// different token". The registration-command form also registers the VM as an
// unmanaged node, which Rancher records as an extra custom-<hash> Machine for
// the same node and IP.
func isRancherNodeBootstrap(script []byte) bool {
	machineBootstrap, _ := os.ReadFile(rancherMachineBootstrapPath)
	return isRancherNodeBootstrapContent(script, machineBootstrap)
}

func isRancherNodeBootstrapContent(script, machineBootstrap []byte) bool {
	trimmed := bytes.TrimSpace(script)
	if len(trimmed) == 0 {
		return false
	}

	// A Rancher-managed machine already has the generated bootstrap script
	// mounted in the same container. An exact match means customScript was
	// copied from it (or points at it).
	if len(machineBootstrap) > 0 && bytes.Equal(trimmed, bytes.TrimSpace(machineBootstrap)) {
		return true
	}

	text := string(trimmed)
	for _, marker := range rancherBootstrapMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// runCustomScript uploads the configured script to the guest over SSH stdin
// and executes it as root (or with passwordless sudo). It runs at the end of
// Create(), after installAuthorizedKey, and before libmachine/Rancher invokes
// its node init script. A non-zero exit fails Create so Rancher does not
// attempt to join a partially configured node.
func (d *Driver) runCustomScript() error {
	script, err := d.customScriptContents()
	if err != nil {
		return err
	}
	if len(script) == 0 {
		log.Infof("FusionCompute: no customScript configured; skipping optional first-boot script")
		return nil
	}
	if isRancherNodeBootstrap(script) {
		log.Warnf("FusionCompute: customScript is Rancher's node init/registration script; skipping it. Rancher runs its own generated bootstrap script after Create(), so running it here would bootstrap the node twice (RKE2 fails with \"bootstrap data already found and encrypted with different token\") and create a duplicate custom-<hash> Machine. Remove the init/registration command from customScript; use it only for host preparation such as growing the disk or installing CA certificates.")
		return nil
	}

	cfg, err := ssh.NewNativeConfig(d.GetSSHUsername(), &ssh.Auth{Keys: []string{d.GetSSHKeyPath()}})
	if err != nil {
		return fmt.Errorf("fusioncompute: build SSH config for customScript: %w", err)
	}
	cfg.Timeout = 20 * time.Second

	client, err := gossh.Dial("tcp", d.sshAddr(), &cfg)
	if err != nil {
		return fmt.Errorf("fusioncompute: connect for customScript at %s: %w", d.sshAddr(), err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("fusioncompute: open customScript session: %w", err)
	}
	defer session.Close()

	command := d.customScriptCommand()
	log.Infof("FusionCompute: running optional customScript first-boot script (%d bytes) as %q over SSH (%s)", len(script), d.GetSSHUsername(), command)

	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		session.Stdin = bytes.NewReader(script)
		out, runErr := session.CombinedOutput(command)
		done <- result{output: out, err: runErr}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			return fmt.Errorf("fusioncompute: customScript failed: %w: %s", res.err, truncateScriptOutput(res.output))
		}
		if len(res.output) > 0 {
			log.Debugf("FusionCompute: customScript output: %s", truncateScriptOutput(res.output))
		}
		log.Infof("FusionCompute: customScript completed")
		return nil
	case <-time.After(customScriptTimeout):
		_ = session.Close()
		_ = client.Close()
		return fmt.Errorf("fusioncompute: customScript timed out after %s", customScriptTimeout)
	}
}

func truncateScriptOutput(out []byte) string {
	if len(out) <= customScriptOutputLimit {
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out[:customScriptOutputLimit])) + "... (truncated)"
}
