/**
 * FusionCompute VRM REST API helper for the node-driver UI.
 *
 * Requests go through Rancher's server-side meta proxy via the shell proxy
 * API (`$shell.proxy`, Rancher 2.15+). `proxy.request()` builds the
 * `/meta/proxy/<host>:<port>/<path>` URL and dispatches it, so the browser
 * never needs direct network access to the FC host and CORS never applies.
 *
 * The proxy only forwards to allow-listed hosts. In Rancher 2.15 the
 * allow-list is authored as `ProxyEndpoint` CRs (`management.cattle.io/v3`,
 * `spec.routes[].domain`). Requests also fail if Rancher does not trust the
 * VRM's TLS certificate — Rancher's proxy verifies it and offers no
 * skip-verify option, unlike the Go driver, where the cloud credential's
 * `skipTlsVerify` checkbox opts out of certificate verification.
 *
 * Auth mirrors the Go driver (client.go): POST /service/session with
 * X-Auth-User / X-Auth-Key, read X-Auth-Token from the response headers,
 * then send X-Auth-Token on every subsequent call.
 *
 * X-Auth-Key carries the SHA-256 hex of the password, never the plaintext:
 * FusionCompute selects the scheme from X-ENCRYPT-ALGORITHM ("0" = SHA-256,
 * "1" = plaintext; 0 is also the server default). The hash is computed with
 * the shell's dependency-free helper so it works in any context.
 *
 * Endpoints verified against the Go driver's own calls and
 * https://github.com/kimzhong/fc-inventory (fc_client.py).
 */

import { sha256 } from '@shell/utils/crypto';

const API_ACCEPT = 'application/json;version=v8.1;charset=UTF-8';
const DEFAULT_PORT = '7443';

// Accept "10.0.0.1", "10.0.0.1:7443", "https://10.0.0.1",
// "https://10.0.0.1:7443/" and return a bare origin. An explicit port in
// dcsHostname wins over dcsPort, matching the Go driver's precedence.
function parseEndpoint(raw, fallbackPort) {
  const trimmed = String(raw || '').trim();
  const withScheme = /^https?:\/\//i.test(trimmed) ? trimmed : `https://${ trimmed }`;
  const url = new URL(withScheme);

  url.port = url.port || String(fallbackPort || DEFAULT_PORT);
  url.pathname = '';
  url.search = '';
  url.hash = '';

  return { origin: url.origin, host: url.hostname, port: url.port };
}

// FC returns site / dvswitch URIs as "/service/sites/1". Tolerate a value
// missing the /service prefix so a hand-entered dcsSiteUri still works.
function normalizeServiceUri(uri) {
  const s = String(uri || '').trim().replace(/\/+$/, '');

  if (!s) {
    return '';
  }

  return s.startsWith('/service/') ? s : `/service/${ s.replace(/^\/+/, '') }`;
}

// FC returns both `urn` (urn:sites:1:vms:i-0001) and `uri`
// (/service/sites/1/vms/i-0001). The driver wants the bare VM id.
function idFrom(item) {
  if (item.urn) {
    return String(item.urn).split(':').pop();
  }
  if (item.uri) {
    return String(item.uri).split('/').filter(Boolean).pop();
  }

  return item.id;
}

// Datastore / port-group flags want the full URN the FC API returned.
function urnFrom(item) {
  return item.urn || item.uri || item.id;
}

function toOptions(list, valueFn, labelFn) {
  return (list || [])
    .map((item) => ({
      label: labelFn ? labelFn(item) : (item.name || idFrom(item)),
      value: valueFn(item),
    }))
    .filter((o) => o.value)
    .sort((a, b) => a.label.localeCompare(b.label));
}

export default class FusionCompute {
  constructor(proxy, opts) {
    if (!proxy) {
      throw new Error('FusionCompute: $shell.proxy is unavailable — the node-driver UI requires Rancher 2.15 or newer');
    }

    const { origin, host } = parseEndpoint(opts.dcsHostname, opts.dcsPort);

    this.proxy = proxy;
    this.origin = origin;
    this.host = host;
    this.username = opts.dcsUsername;
    this.password = opts.dcsPassword;
    this.siteUri = normalizeServiceUri(opts.dcsSiteUri);
    this.token = '';
  }

  // Absolute URL for an FC path such as "/service/session" or
  // "/service/sites/1/vms". Passed to proxy.request(), which strips the
  // scheme to build the /meta/proxy path.
  url(path) {
    return new URL(path, this.origin);
  }

  async request(path, { method = 'GET', headers = {}, data } = {}) {
    return this.proxy.request({
      url:    this.url(path),
      method,
      accept: API_ACCEPT,
      headers,
      data,
    });
  }

  async login() {
    if (this.token) {
      return;
    }

    const res = await this.request('/service/session', {
      method:  'POST',
      headers: {
        'Content-Type':        'application/json;charset=UTF-8',
        'Accept-Language':     'en_US',
        'X-Auth-User':         this.username,
        'X-Auth-Key':          sha256(this.password, 'hex'),
        'X-Auth-UserType':     '0',
        'X-ENCRYPT-ALGORITHM': '0',
      },
    });

    const token = res?._headers?.['x-auth-token'];

    if (!token) {
      throw new Error(`FusionCompute login failed: ${ await this.describeFailure(res) }`);
    }

    this.token = token;
  }

  // Rancher's proxy surfaces both "host not allow-listed" and "upstream
  // unreachable" (including a TLS trust failure) as a failed request, so
  // spell out both possibilities rather than guessing one.
  async describeFailure(res) {
    const status = res?._status ? `HTTP ${ res._status }` : 'no response';
    const allowed = await this.proxy
      .isDomainAllowed(this.host)
      .catch(() => true);

    if (allowed === false) {
      return `${ status } — ${ this.host } is not in Rancher's proxy allow-list; add a ProxyEndpoint CR for it`;
    }

    return `${ status } from ${ this.origin } — check the credentials, and that Rancher trusts the VRM's TLS certificate`;
  }

  async get(path) {
    await this.login();

    return this.request(path, { headers: { 'X-Auth-Token': this.token } });
  }

  async ensureSite() {
    if (this.siteUri) {
      return this.siteUri;
    }
    const res = await this.get('/service/sites');
    const site = (res && res.sites && res.sites[0]);

    if (!site) {
      throw new Error('FusionCompute: no sites returned');
    }
    this.siteUri = normalizeServiceUri(site.uri);

    return this.siteUri;
  }

  async getDatastoreOptions() {
    const site = await this.ensureSite();
    const res = await this.get(`${ site }/datastores`);

    return toOptions(res && res.datastores, urnFrom);
  }

  // Templates are VMs with isTemplate = true. The driver clones from the
  // bare VM id (client.go CloneVM), so the option value is the id.
  async getTemplateOptions() {
    const site = await this.ensureSite();
    const res = await this.get(`${ site }/vms`);
    const all = (res && res.vms) || [];
    const templates = all.filter((v) => v.isTemplate);

    return toOptions(templates, idFrom, (v) => `${ v.name } (${ idFrom(v) })`);
  }

  // Clusters are the "location" the driver places the VM in. The CloneSpec
  // `location` field takes the full cluster URN (client.go), so the option
  // value is the urn and the label is the cluster name.
  async getClusterOptions() {
    const site = await this.ensureSite();
    const res = await this.get(`${ site }/clusters`);

    return toOptions(res && res.clusters, urnFrom);
  }

  async getPortGroupOptions() {
    const site = await this.ensureSite();

    // FC exposes port groups both site-wide and per-dvswitch; prefer the
    // site-wide list and fall back to walking dvswitchs.
    try {
      const res = await this.get(`${ site }/portgroups`);

      if (res && res.portgroups && res.portgroups.length) {
        return toOptions(res.portgroups, urnFrom);
      }
    } catch (e) {
      // fall through to dvswitch walk
    }

    const dvres = await this.get(`${ site }/dvswitchs`);
    const switches = (dvres && (dvres.dvswitchs || dvres.dvSwitchs)) || [];
    let out = [];

    for (const dv of switches) {
      // FC gives the dvswitch's API path as `uri`; a `urn` cannot form a
      // URL path, so it is not a usable fallback here.
      const uri = normalizeServiceUri(dv.uri);

      if (!uri) {
        continue;
      }
      const pgres = await this.get(`${ uri }/portgroups`);
      const pgs = (pgres && (pgres.portgroups || pgres.portGroups)) || [];

      out = out.concat(toOptions(pgs, urnFrom));
    }

    return out;
  }
}
