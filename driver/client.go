package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rancher/machine/libmachine/log"
)

// lastClonePayloadPath is where we persist the most recent clone
// request body so that even when docker-machine's log plumbing hides
// the plugin's stderr, the payload is always inspectable after a
// failed `docker-machine create -d fusioncompute`.
const lastClonePayloadPath = "/tmp/fusioncompute-last-clone-payload.json"

// lastAuthDebugPath holds the last /service/session request context
// (URL, sanitized headers) so a 401 with an empty error body — which
// is ambiguous on its own — can be diagnosed after the fact.
const lastAuthDebugPath = "/tmp/fusioncompute-last-auth-debug.json"

// FusionCompute REST API surface used by this driver. The constants are
// kept here so that future API version changes are a one-line edit.
const (
	fcAPIVersion = "application/json;version=v8.1;charset=UTF-8"
	fcLang       = "en_US"
)

// fcClient is a thin wrapper around net/http that handles the parts of
// FusionCompute we care about: session auth, site resolution, VM
// lifecycle, and async task polling.
//
// It is intentionally *not* an exhaustive SDK — only the endpoints the
// docker-machine driver needs are implemented. Adding more is mechanical.
type fcClient struct {
	hostname string // "host" or "host:port"; if no :port present, `port` is appended
	port     int    // 0 means "leave hostname alone"; otherwise appended when hostname lacks :port
	username string
	password string

	// siteURI is "/service/sites/<id>" or the full path returned by the
	// sites endpoint, e.g. "/service/sites/1". Resolved lazily on first
	// call to ensureSite().
	siteURI string

	token string // X-Auth-Token returned by /service/session

	hc *http.Client
}

// VM is the subset of fields we read from FusionCompute's VM object.
// All other fields returned by the API are ignored on purpose.
//
// The guest address FusionCompute reports lives on the NIC:
// vmConfig.nics[].ip. There is no reliable top-level `ip` on this build, so
// use GuestIP() rather than reading IP directly.
type VM struct {
	URI        string `json:"uri"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // e.g. "running", "stopped", "creating"
	PowerState string `json:"powerState"` // some endpoints put power here
	IP         string `json:"ip"`         // legacy top-level fallback (usually empty here)
	VMConfig   struct {
		NICs []VMNIC `json:"nics"`
	} `json:"vmConfig"`
}

// VMNIC is the subset of a VM's NIC spec needed to discover the guest IP.
type VMNIC struct {
	SequenceNum int    `json:"sequenceNum"`
	IP          string `json:"ip"`
}

// GuestIP returns the first usable guest IPv4 address FusionCompute reports
// for the VM. It prefers the primary NIC (sequenceNum 0), then any NIC that
// has an address, then the legacy top-level `ip`.
//
// FC reports "0.0.0.0" (or an empty string) until the guest has actually
// obtained an address, so those are treated as "not ready" and skipped —
// callers should keep polling rather than accepting 0.0.0.0.
func (v *VM) GuestIP() string {
	if v == nil {
		return ""
	}
	for _, n := range v.VMConfig.NICs {
		if n.SequenceNum == 0 {
			if ip := usableGuestIP(n.IP); ip != "" {
				return ip
			}
		}
	}
	for _, n := range v.VMConfig.NICs {
		if ip := usableGuestIP(n.IP); ip != "" {
			return ip
		}
	}
	return usableGuestIP(v.IP)
}

// usableGuestIP normalises a guest address and reports "" while it is not a
// real, routable address. "0.0.0.0" (and the IPv6 unspecified "::") are the
// placeholders FC returns before DHCP/either stack assigns an address.
func usableGuestIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" || ip == "0.0.0.0" || ip == "::" {
		return ""
	}
	return ip
}

// Task is the asynchronous response shape used both for the clone
// POST response (which carries the new VM URN and a TaskURI to poll)
// and for the per-poll responses.
//
// From the Ansible playbook
// (`the reference provisioning workflow`):
//
//	'vmid':   dcs_provision.results[item_index].json.uri      // new VM URN
//	'taskUri': dcs_provision.results[item_index].json.taskUri // poll this
//
// So we model BOTH the URI (VM, when relevant) and the TaskURI
// (asynchronous tracker to poll). Status/Progress are the standard
// FC task polling fields.
type Task struct {
	URI      string `json:"uri,omitempty"`     // VM URN (for clone response)
	TaskURI  string `json:"taskUri,omitempty"` // poll URI
	Status   string `json:"status"`            // "success", "failed", "running", ...
	Progress int    `json:"progress"`          // 0-100
	Result   string `json:"result,omitempty"`  // task's `result` field (used by some endpoints)
}

// CloneSpec is the JSON body posted to
// {site}/vms/{template}/action/clone. It mirrors the Ansible
// playbook's `server_spec` block in the reference provisioning workflow
// almost exactly so that the JSON the FC schema validator sees is
// indistinguishable from a body produced by the working playbook.
//
// The fields with `omitempty` are only used when the user explicitly
// supplies them via driver flags (Location). The playbook always sends
// every field, so structurally we always emit a value — but that one
// genuinely optional input from our CLI is the only one we elide when
// empty.
//
// Important details gleaned from the actual working playbook body
// captured on the FC farm:
//
//   - cpu.quantity / cpu.weight / memory.quantityMB / memory.weight
//     are sent as JSON strings, not numbers. Ansible's `to_json`
//     filter emits quoted numerics for templated values, and FC's
//     schema validator accepts the string form. Using Go int here
//     caused errorCode 10000001 ("请求消息格式非法") on this build.
//   - vmConfig.properties is required. The playbook always sends
//     three explicit `null` sub-fields (bootOptions, isEnableHa,
//     vmFaultProcess). Omitting the block entirely was wrong; the
//     block must be present with null sub-fields.
//   - vmCustomization.vmcustomizationId is JSON `null` (not "").
//     We use interface{} so it always emits `null`.
//   - vmCustomization.password is always sent by the reference body,
//     even when empty.
type CloneSpec struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Group         string `json:"group"`
	Location      string `json:"location,omitempty"`
	IsBindingHost string `json:"isBindingHost"`
	VMConfig      struct {
		CPU struct {
			Quantity        string `json:"quantity"`
			CoresPerSocket  int    `json:"coresPerSocket"`
			Reservation     int    `json:"reservation"`
			Weight          string `json:"weight"`
			Limit           int    `json:"limit"`
			CPUHotPlug      int    `json:"cpuHotPlug"`
			CPUPolicy       string `json:"cpuPolicy"`
			CPUThreadPolicy string `json:"cpuThreadPolicy"`
		} `json:"cpu"`
		Memory struct {
			QuantityMB  string `json:"quantityMB"`
			Reservation int    `json:"reservation"`
			Weight      string `json:"weight"`
			HugePage    string `json:"hugePage"`
			Limit       int    `json:"limit"`
			MemHotPlug  int    `json:"memHotPlug"`
		} `json:"memory"`
		Disks []struct {
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
		} `json:"disks"`
		NICs []struct {
			Name             string `json:"name"`
			PortGroupURN     string `json:"portGroupUrn"`
			SequenceNum      int    `json:"sequenceNum"`
			VirtIO           int    `json:"virtIo"`
			BootOrder        int    `json:"bootOrder"`
			ConnectAtPowerOn bool   `json:"connectAtPowerOn"`
		} `json:"nics"`
		Properties struct {
			BootOptions    interface{} `json:"bootOptions"`
			IsEnableHA     interface{} `json:"isEnableHa"`
			VMFaultProcess interface{} `json:"vmFaultProcess"`
		} `json:"properties"`
	} `json:"vmConfig"`
	AutoBoot        bool `json:"autoBoot"`
	VMCustomization struct {
		OSType             string             `json:"osType"`
		UpdatePassword     bool               `json:"updatePassword"`
		VMCustomizationID  interface{}        `json:"vmcustomizationId"`
		NICSpecification   []NICSpecification `json:"nicSpecification"`
		Hostname           string             `json:"hostname"`
		Password           string             `json:"password"`
		IsUpdateVMPassword bool               `json:"isUpdateVmPassword"`
	} `json:"vmCustomization"`
}

// NICSpecification is a single entry in vmCustomization.nicSpecification.
//
// The driver always uses DHCP: a bare `{ipVersion:4, sequenceNum:1}` entry
// tells FusionCompute to let the guest obtain its address at boot. FC rejects
// an empty/absent nicSpecification on this build, so exactly one minimal entry
// is always sent.
type NICSpecification struct {
	IPVersion   int `json:"ipVersion"`
	SequenceNum int `json:"sequenceNum"`
}

// newClient builds an http.Client for the FusionCompute API. TLS
// verification is controlled by the credential's verifyTls option; it
// defaults to off because FusionCompute deployments commonly use self-signed
// VRM certificates (matching the Ansible playbook's `validate_certs: false`).
func (d *Driver) newClient() (*fcClient, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !d.VerifyTLS},
	}
	return &fcClient{
		hostname: d.DCSHostname,
		port:     d.DCSPort,
		username: d.DCSUsername,
		password: d.DCSPassword,
		siteURI:  d.DCSSiteURI,
		hc: &http.Client{
			Transport: tr,
			Timeout:   60 * time.Second,
		},
	}, nil
}

// baseURL is the root used for both auth and site resolution.
func (c *fcClient) baseURL() string {
	return "https://" + c.hostPort() + "/service"
}

// hostPort returns c.hostname with :port appended when c.hostname
// contains no :port and c.port > 0. This lets users pass either
// "dcs.example.com" or "dcs.example.com:7443" for --fusioncompute-dcs-hostname.
func (c *fcClient) hostPort() string {
	if c.port <= 0 || strings.Contains(c.hostname, ":") {
		return c.hostname
	}
	return fmt.Sprintf("%s:%d", c.hostname, c.port)
}

// auth performs the POST /service/session login and stores the resulting
// X-Auth-Token. Mirrors the playbook's "Authentication Gather
// X-Auth-Token" task exactly.

// logAuthRequest dumps the /service/session request context (URL,
// sanitized headers) to libmachine debug logs and to a file so that a
// 401 with an empty FC errorDes can be diagnosed after the fact. The
// password is never written — we only record its length so an operator
// can tell whether the env var was empty / shell-escaped.
func (c *fcClient) logAuthRequest(req *http.Request) {
	log.Debugf("fusioncompute auth: %s %s", req.Method, req.URL.String())
	for k, v := range req.Header {
		switch k {
		case "X-Auth-Key":
			log.Debugf("  %s: <redacted len=%d>", k, len(strings.Join(v, "")))
		default:
			log.Debugf("  %s: %s", k, strings.Join(v, ","))
		}
	}

	dump := struct {
		URL    string            `json:"url"`
		Method string            `json:"method"`
		Header map[string]string `json:"header"`
	}{URL: req.URL.String(), Method: req.Method}
	hdr := map[string]string{}
	for k, v := range req.Header {
		if k == "X-Auth-Key" {
			hdr[k] = fmt.Sprintf("<redacted len=%d>", len(strings.Join(v, "")))
			continue
		}
		hdr[k] = strings.Join(v, ",")
	}
	dump.Header = hdr

	if dir := filepath.Dir(lastAuthDebugPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if raw, err := json.MarshalIndent(dump, "", "  "); err == nil {
		if err := os.WriteFile(lastAuthDebugPath, raw, 0o644); err != nil {
			log.Debugf("fusioncompute auth: could not write %s: %v", lastAuthDebugPath, err)
		} else {
			log.Debugf("fusioncompute auth: request context persisted to %s", lastAuthDebugPath)
		}
	}
}

// logAuthFailure dumps the response body and headers of a failed
// /service/session call so we can see whether FC included any
// diagnostic hints (X-Error-*, WWW-Authenticate, etc.).
func logAuthFailure(req *http.Request, resp *http.Response, body []byte) {
	log.Errorf("fusioncompute auth failed: %s %s -> status=%d body=%s",
		req.Method, req.URL.String(), resp.StatusCode, string(body))
	for k, v := range resp.Header {
		log.Errorf("  auth response header %s: %s", k, strings.Join(v, ","))
	}
	respDump := lastAuthDebugPath + ".response"
	if dir := filepath.Dir(respDump); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(respDump, body, 0o644); err == nil {
		log.Errorf("fusioncompute auth: response body persisted to %s", respDump)
	}
}

func (c *fcClient) auth() error {
	if c.token != "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL()+"/session", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", fcAPIVersion)
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept-Language", fcLang)
	req.Header.Set("X-Auth-User", c.username)
	req.Header.Set("X-Auth-Key", c.password)
	req.Header.Set("X-Auth-UserType", "0")
	req.Header.Set("X-ENCRYPT-ALGORITHM", "1")

	c.logAuthRequest(req)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fusioncompute auth: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		logAuthFailure(req, resp, body)
		return fmt.Errorf("fusioncompute auth: status=%d body=%s", resp.StatusCode, string(body))
	}
	tok := resp.Header.Get("X-Auth-Token")
	if tok == "" {
		return errors.New("fusioncompute auth: empty X-Auth-Token in response")
	}
	c.token = tok
	return nil
}

// ensureSite resolves the site URI if not pre-supplied by listing the
// available sites and taking the first.
func (c *fcClient) ensureSite() error {
	if c.siteURI != "" {
		return nil
	}
	if err := c.auth(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL()+"/sites", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", fcAPIVersion)
	req.Header.Set("Accept-Language", fcLang)
	req.Header.Set("X-Auth-Token", c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fusioncompute list sites: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fusioncompute list sites: status=%d body=%s", resp.StatusCode, string(body))
	}

	// Response shape: { "sites": [ { "uri": "/service/sites/1", "name": "..." } ] }
	var out struct {
		Sites []struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		} `json:"sites"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("fusioncompute list sites: decode: %w (body=%s)", err, string(body))
	}
	if len(out.Sites) == 0 {
		return errors.New("fusioncompute list sites: no sites returned")
	}
	c.siteURI = out.Sites[0].URI
	return nil
}

// doJSON is the shared request helper for authenticated, site-scoped calls.
// path is the *full* path (it may be a relative site path like
// "/service/sites/1/vms/..." returned from clone, in which case it is
// prefixed with https://<host>).
//
// For diagnostics, the full request body is:
//   - logged via libmachine/log.Debugf (visible with --debug), and
//   - persisted to lastClonePayloadPath so it can be inspected after
//     a failed `docker-machine create`, regardless of whether the
//     plugin's stderr was routed back to the terminal.
//
// On any non-2xx response we also persist the response body and log
// it via libmachine/log.Errorf so docker-machine surfaces it.
func (c *fcClient) doJSON(method, path string, body interface{}, out interface{}) error {
	if err := c.auth(); err != nil {
		return err
	}
	var rdr io.Reader
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequest(method, c.absURL(path), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", fcAPIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	req.Header.Set("Accept-Language", fcLang)
	req.Header.Set("X-Auth-Token", c.token)

	// Persist + log the request payload. The path is included so the
	// dump self-identifies which call it belongs to.
	log.Debugf("fusioncompute request: %s %s", method, req.URL.String())
	for k, v := range req.Header {
		if k == "X-Auth-Key" || k == "X-Auth-Token" {
			log.Debugf("  %s: <redacted>", k)
			continue
		}
		log.Debugf("  %s: %s", k, strings.Join(v, ","))
	}
	if len(rawBody) > 0 {
		// Pretty-print for readability and persist as-is to disk so
		// we can inspect the bytes the server actually saw.
		var pp []byte
		var pretty interface{}
		if err := json.Unmarshal(rawBody, &pretty); err == nil {
			if p, err := json.MarshalIndent(pretty, "", "  "); err == nil {
				pp = p
			}
		}
		if pp == nil {
			pp = rawBody
		}
		log.Debugf("  body:\n%s", string(pp))
		if dir := filepath.Dir(lastClonePayloadPath); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		if err := os.WriteFile(lastClonePayloadPath, pp, 0o644); err != nil {
			log.Debugf("fusioncompute: could not write payload to %s: %v", lastClonePayloadPath, err)
		} else {
			log.Debugf("fusioncompute: request body persisted to %s", lastClonePayloadPath)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fusioncompute %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Persist + log the failure body and headers so we can see
		// what FC didn't like about the request. FC sometimes returns
		// an X-Error-* header pointing at the offending field.
		log.Errorf("fusioncompute response: %s %s -> status=%d body=%s",
			method, req.URL.String(), resp.StatusCode, string(respBody))
		for k, v := range resp.Header {
			log.Errorf("  response header %s: %s", k, strings.Join(v, ","))
		}
		if dir := filepath.Dir(lastClonePayloadPath); dir != "" {
			_ = os.MkdirAll(dir, 0o755)
		}
		respDump := lastClonePayloadPath + ".response"
		if err := os.WriteFile(respDump, respBody, 0o644); err == nil {
			log.Errorf("fusioncompute: response body persisted to %s", respDump)
		}
		return fmt.Errorf("fusioncompute %s %s: status=%d body=%s",
			method, path, resp.StatusCode, string(respBody))
	}
	if c.token == "" && resp.Header.Get("X-Auth-Token") != "" {
		// Auth response — confirm we got a token (defensive — auth()
		// is the canonical path, but this catches manual session calls).
		log.Debugf("fusioncompute: observed X-Auth-Token from %s response (length=%d)",
			method, len(resp.Header.Get("X-Auth-Token")))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("fusioncompute %s %s: decode: %w (body=%s)",
				method, path, err, string(respBody))
		}
	}
	return nil
}

// absURL resolves a path returned by the API (which may be site-scoped,
// e.g. "/service/sites/1/vms/...") against the configured hostname.
func (c *fcClient) absURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return "https://" + c.hostPort() + path
}

// CloneVM clones the template (or source VM) identified by `templateID`
// using `spec` and returns the resulting task URI plus the eventual VM
// URN once the task completes.
func (c *fcClient) CloneVM(templateID string, spec *CloneSpec) (*Task, error) {
	if err := c.ensureSite(); err != nil {
		return nil, err
	}
	path := c.siteURI + "/vms/" + templateID + "/action/clone"
	var t Task
	if err := c.doJSON(http.MethodPost, path, spec, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// WaitTask polls a task URI until it terminates, returning the final
// Task. retries*delay is the maximum wait. Mirrors the playbook's
// `until: progress == 100 and status == "success"` loop with a sane
// upper bound.
func (c *fcClient) WaitTask(taskURI string, retries int, delay time.Duration) (*Task, error) {
	var last Task
	for i := 0; i < retries; i++ {
		var t Task
		if err := c.doJSON(http.MethodGet, taskURI, nil, &t); err != nil {
			return nil, err
		}
		last = t
		if t.Progress == 100 && (t.Status == "success" || t.Status == "failed") {
			return &t, nil
		}
		time.Sleep(delay)
	}
	return &last, fmt.Errorf("fusioncompute: task %s did not complete in %d retries", taskURI, retries)
}

// GetVM returns a single VM by URN.
func (c *fcClient) GetVM(vmURI string) (*VM, error) {
	var v VM
	if err := c.doJSON(http.MethodGet, vmURI, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// PowerAction issues a generic VM power action (start/stop/reboot/force-off).
// Action names match the FusionCompute REST verbs.
func (c *fcClient) PowerAction(vmURI, action string) (*Task, error) {
	path := vmURI + "/action/" + action
	var t Task
	if err := c.doJSON(http.MethodPost, path, struct{}{}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteVM removes a VM. Per the FusionCompute v8.x REST API, the
// delete operation is a plain HTTP DELETE on the VM URI itself:
//
//	DELETE /service/sites/{site_id}/vms/{vm_id}
//
// (NOT POST {vmURI}/action/delete — that pattern was inherited from
// the playbook's stop/start action verbs and is wrong for delete.)
//
// Some FusionCompute builds require the VM to be stopped before delete
// succeeds; callers should call Stop() first to be safe.
//
// The response is typically 204 No Content (or 200 with an empty
// body), so we pass nil as the decode target. Any 2xx is treated as
// success.
func (c *fcClient) DeleteVM(vmURI string) error {
	return c.doJSON(http.MethodDelete, vmURI+"?isForce=true", nil, nil)
}
