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
		return nil, fmt.Errorf("fusioncompute: custom script is %d bytes, maximum is %d", len(script), maxCustomScriptBytes)
	}
	return script, nil
}

func readCustomScriptFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("fusioncompute: stat custom script file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("fusioncompute: custom script file %s is not a regular file", path)
	}
	if info.Size() > maxCustomScriptBytes {
		return nil, fmt.Errorf("fusioncompute: custom script file %s is %d bytes, maximum is %d", path, info.Size(), maxCustomScriptBytes)
	}
	script, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fusioncompute: read custom script file %s: %w", path, err)
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
		return nil
	}

	cfg, err := ssh.NewNativeConfig(d.GetSSHUsername(), &ssh.Auth{Keys: []string{d.GetSSHKeyPath()}})
	if err != nil {
		return fmt.Errorf("fusioncompute: build SSH config for custom script: %w", err)
	}
	cfg.Timeout = 20 * time.Second

	client, err := gossh.Dial("tcp", d.sshAddr(), &cfg)
	if err != nil {
		return fmt.Errorf("fusioncompute: connect for custom script at %s: %w", d.sshAddr(), err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("fusioncompute: open custom script session: %w", err)
	}
	defer session.Close()

	command := d.customScriptCommand()
	log.Infof("FusionCompute: running custom script (%d bytes) as %q over SSH (%s)", len(script), d.GetSSHUsername(), command)

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
			return fmt.Errorf("fusioncompute: custom script failed: %w: %s", res.err, truncateScriptOutput(res.output))
		}
		if len(res.output) > 0 {
			log.Debugf("FusionCompute: custom script output: %s", truncateScriptOutput(res.output))
		}
		log.Infof("FusionCompute: custom script completed")
		return nil
	case <-time.After(customScriptTimeout):
		_ = session.Close()
		_ = client.Close()
		return fmt.Errorf("fusioncompute: custom script timed out after %s", customScriptTimeout)
	}
}

func truncateScriptOutput(out []byte) string {
	if len(out) <= customScriptOutputLimit {
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out[:customScriptOutputLimit])) + "... (truncated)"
}
