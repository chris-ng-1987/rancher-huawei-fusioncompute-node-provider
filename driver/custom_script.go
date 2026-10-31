package main

import (
	"bytes"
	"encoding/base64"
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

// customScriptContents returns the shell script to run, decoding it when
// --fusioncompute-custom-script-base64 is set. A blank script means "do
// nothing".
//
// Plain text is the default because the driver writes the script to the
// remote bash process over SSH stdin; no shell interpolation is involved, so
// quotes, `$`, and newlines are preserved. The optional base64 flag exists
// for API/CLI callers that cannot transport multiline values reliably. Base64
// is only an encoding; it does not make the script secret.
func (d *Driver) customScriptContents() ([]byte, error) {
	if strings.TrimSpace(d.CustomScript) == "" {
		return nil, nil
	}

	if !d.CustomScriptBase64 {
		return []byte(d.CustomScript), nil
	}

	// Accept line-wrapped base64 (common when a value has been copied from a
	// terminal or a YAML block scalar); standard base64 decoders reject
	// embedded whitespace.
	encoded := strings.Join(strings.Fields(d.CustomScript), "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("fusioncompute: decode --fusioncompute-custom-script as base64: %w", err)
	}
	if len(decoded) == 0 {
		return nil, nil
	}
	return decoded, nil
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
