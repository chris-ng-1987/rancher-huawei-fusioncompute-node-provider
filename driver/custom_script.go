package main

import (
	"bytes"
	"fmt"
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
)

// customScriptContents returns the shell script to run. A blank script means
// "do nothing".
//
// The script is written to the remote bash process over SSH stdin, so no
// shell interpolation is involved; quotes, `$`, and newlines are preserved.
// The value is plain text, matching the Rancher UI textarea and the machine
// config field. It is not a place for secrets; use a secret/URL-based
// mechanism for confidential data.
func (d *Driver) customScriptContents() []byte {
	if strings.TrimSpace(d.CustomScript) == "" {
		return nil
	}
	return []byte(d.CustomScript)
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
	script := d.customScriptContents()
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
