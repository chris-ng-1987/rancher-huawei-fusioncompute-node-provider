# ui — Rancher UI extension for the FusionCompute node driver

Rancher UI extension for the `docker-machine-driver-fusioncompute` node driver
(Rancher 2.15+ / shell v3, Vue 3).

When loaded, the extension renders the machine-pool form grouped into sections:

- **Machine** — `cpu`, `memoryMb`, `diskGb` (numeric `UnitInput`s).
- **Placement** — `templateId`, `datastoreUrn`, `location` (live `LabeledSelect`s).
- **Network** — `portGroupUrn` (live `LabeledSelect`); the guest NIC always uses DHCP.
- **Access** — choose an **Authentication** method:
  1. **SSH private key** → `sshUser` + `sshPrivateKey` shown. The private key must be
     unencrypted; the driver writes it to the machine store and uses it for the initial
     login. The template must already trust the matching public key. No password is sent
     to FusionCompute. Selecting this mode clears `sshPassword`, `setVmPassword`, and
     `generatePassword`.
  2. **Username and password**, `setVmPassword` off → `sshUser` + `sshPassword` shown,
     where `sshPassword` is the template's *existing* credential, used once by the driver
     to install its key (leave blank if the template already trusts the key).
  3. **Username and password**, `setVmPassword` on, `generatePassword` on → the driver
     generates a random password (stored on the node); no password field is shown.
  4. **Username and password**, `setVmPassword` on, `generatePassword` off →
     `sshPassword` is the *new* password FusionCompute resets on first boot.

  When reset is on, `sshUser` is hidden: FusionCompute resets the **root** account (the
  clone API has no username field and the reset is only permitted for root), and the
  driver forces the SSH user to root for the key install and later logins. Selecting the
  username/password mode clears `sshPrivateKey`.
- **Advanced** — everything else via the same `<Questions>` renderer the generic form
  uses (`vmDescription`, etc.).

Cloud-credential fields (`dcsHostname`, `dcsPort`, `dcsUsername`, `dcsPassword`,
`dcsSiteUri`) are excluded so they aren't duplicated. `sshPort` is hidden because SSH is
always on port 22, and `sshKeyPath` is hidden because the Rancher UI provides
`sshPrivateKey` instead of a server-local path. Standalone docker-machine users can still
set `--fusioncompute-ssh-key-path`.

## Layout

```
ui/
├── package.json               # dev skeleton: deps + `yarn build-pkg`
├── vue.config.js              # @rancher/shell/vue.config wrapper
├── babel.config.js
├── tsconfig.json
├── scripts/
│   └── build-extension.sh     # drives @rancher/shell build-pkg.sh + arranges UIPlugin layout
└── pkg/
    └── fusioncompute/         # the extension package Rancher loads
        ├── package.json       # name/version + rancher annotations + @vue/cli-plugin-* (REQUIRED)
        ├── index.js           # entry: importTypes(plugin) + plugin.metadata
        ├── fusioncompute.js   # FusionCompute REST helper used by the live dropdowns
        ├── vue.config.js      # require('./.shell/pkg/vue.config')(__dirname)
        ├── babel.config.js    # require('./.shell/pkg/babel.config.js')
        └── machine-config/
            └── fusioncompute.vue   # the form (component name == NodeDriver name)
```

The file name under `machine-config/` **must** equal the driver name (`fusioncompute`) —
the shell auto-registers it as `machine-config/fusioncompute`.

## Build

Node 24 is required by `@rancher/shell@3`. With Node 24 and yarn available locally:

```bash
corepack enable
yarn install
yarn build-pkg fusioncompute          # or: bash ./scripts/build-extension.sh
```

To build without installing Node locally, run the same commands in a container:

```bash
docker run --rm -v "$PWD":/ui -w /ui node:24-bookworm-slim \
  sh -c 'corepack enable && yarn install --network-timeout 600000 && bash ./scripts/build-extension.sh'
```

### Output

`scripts/build-extension.sh` calls Rancher's own `@rancher/shell/scripts/build-pkg.sh`,
then rearranges its output into the layout Rancher's `ui-plugin-catalog` controller
expects:

```
dist-pkg/extensions/fusioncompute/<version>/
├── files.txt
└── plugin/
    ├── fusioncompute-<version>.umd.min.js                 # main entry
    ├── fusioncompute-<version>.umd.min.<hash>.js          # shared deps chunk
    ├── fusioncompute-<version>.umd.min.machine-config.js  # the form (lazy chunk)
    └── package.json                                       # { rancher: true, main: ... }
```

Reference: <https://github.com/rancher/ui-plugin-examples/tree/main/extensions/node-driver/1.0.4>

### Two things that must not be changed

1. **`pkg/fusioncompute/package.json` must declare `@vue/cli-plugin-babel`,
   `@vue/cli-service` and `@vue/cli-plugin-typescript` under `devDependencies`.**
   vue-cli only auto-detects plugins listed in the *package being built*. Without
   `@vue/cli-plugin-typescript` the build fails with
   `Can't resolve '@components/Form/LabeledInput'` because that path resolves to an
   `index.ts`.
2. **Do not add `ARG UI_SKIP`-style skip flags.** Both stages always build.

## Deploy

The extension is delivered as a `catalog.cattle.io/v1 UIPlugin` CR (not
`NodeDriver.spec.uiUrl` — that only loads the `.css` companion on the 2.15 dashboard).
Edit the endpoint in the repo's `deploy/uiplugin.yaml` and apply:

```bash
kubectl --context=<rancher-local> apply -f ../deploy/uiplugin.yaml
kubectl -n cattle-ui-plugin-system get uiplugin fusioncompute -w   # pending -> cached
```

`endpoint` points at the **directory** containing `files.txt` + `plugin/`. Tagged releases
publish that directory to GitHub Pages; you can also copy `ui/dist-pkg/extensions/` to any
static file host and point the endpoint there.

## Live dropdowns (Template / Datastore / Cluster / Port Group)

`templateId`, `datastoreUrn`, `location` and `portGroupUrn` are rendered as
`LabeledSelect` and populated from the FusionCompute API at form load:

1. Read the cloud credential (`rancher/find` on `NORMAN.CLOUD_CREDENTIAL`) for
   `dcsHostname` / `dcsPort` / `dcsUsername` / `dcsSiteUri`.
2. Read `dcsPassword` from the k8s Secret (`management/find` on `SECRET`, base64-decoded)
   — the credential object itself never returns it.
3. `POST /service/session` with `X-Auth-User` / `X-Auth-Key`, read `X-Auth-Token` from the
   response headers.
4. `GET` the lists and map them to options. See `pkg/fusioncompute/fusioncompute.js`.

Requests go through **`$shell.proxy.request()`** (Rancher 2.15+), which builds the
`/meta/proxy/<host>:<port>/…` URL and dispatches it. The helper passes a `URL` and lets the
shell strip the scheme, so an explicit port in `dcsHostname` wins over `dcsPort` instead of
being appended twice.

| Flag | FC request | Option value |
|---|---|---|
| `templateId` | `GET {site}/vms`, filtered to `isTemplate` | VM **id** (last URN segment) |
| `datastoreUrn` | `GET {site}/datastores` | full `urn` |
| `location` | `GET {site}/clusters` | full `urn` (cluster URN) |
| `portGroupUrn` | `GET {site}/portgroups` (falls back to walking `{site}/dvswitchs/{id}/portgroups`) | full `urn` |

Option values match exactly what the Go driver expects (`driver/client.go` `CloneVM` uses
the bare VM id; the disk/NIC specs take full URNs).

### Required: proxy allow-list

The browser talks to FC through Rancher's server-side `/meta/proxy`, which only forwards
to allow-listed hosts. In Rancher 2.15 the allow-list is authored as **`ProxyEndpoint`
CRs** (`management.cattle.io/v3`, `spec.routes[].domain`); see
[`deploy/proxyendpoint.yaml`](../deploy/proxyendpoint.yaml).

**A bare IP cannot be allow-listed.** Rancher validates each `spec.routes[].domain`
against a regex whose final label must begin with a letter, so an IP-addressed VRM needs a
DNS name that resolves from the Rancher server. That name goes in both the `ProxyEndpoint`
and the cloud credential's `dcsHostname`.

### Required: Rancher must trust the VRM's TLS certificate

Rancher's proxy verifies the upstream certificate and has no skip-verify option (unlike
the Go driver's `InsecureSkipVerify`). A VRM with a self-signed certificate fails with a
TLS trust error, and the certificate must also name the host you dial (a factory VRM's
generated `vrmcaserver<hex>` name does not match). Either re-issue the VRM certificate
with the DNS name in its SAN, or put a TLS-terminating reverse proxy in front of the VRM
and use that name as the credential's host.

If you need to install a CA in Rancher, drop it where the container's TLS stack reads it.
In `rancher/rancher` the image sets `SSL_CERT_DIR=/etc/rancher/ssl`, and Go reads every
file in `SSL_CERT_DIR` as a trust anchor while still reading the distro bundle. Read the
real paths off the container:

```bash
docker exec rancher sh -c 'env | grep -iE "ssl_cert|cert_dir|cacert"'
docker exec rancher curl -v -sS -o /dev/null https://<vrm>:7443/service/session 2>&1 \
  | grep -E 'CAfile|CApath'
```

### Diagnosing failures

Allow-list denial and an upstream/TLS failure both surface to the browser as a failed
request, so use Rancher's server log to tell them apart:

| Log line | Cause |
|---|---|
| `Failed to proxy: invalid host: <host>` | Not allow-listed |
| `http: proxy error: x509: certificate signed by unknown authority` | TLS trust failure |

The form still renders when lookups fail: the selects fall back to showing whatever value
is already stored, and a warning banner shows the error.

## How the form is composed

Rancher renders **either** the custom component **or** the shell's
`machine-config/generic.vue` — never both (`MachinePool.vue`'s `configComponent` computed).
So `fusioncompute.vue` replicates the generic behaviour:

1. Resolves the driver's schema via `plugins/fieldsForDriver`.
2. Computes the cloud-credential field names (`credentialFieldForDriver` /
   `fieldNamesForDriver`) and excludes them.
3. Adds `cpu`, `memoryMb`, `diskGb` to that ignore-list and renders them itself as
   `UnitInput`s.
4. Hands everything else to `<Questions>`, i.e. all remaining driver flags render as they
   would in the generic form.

### Adding another custom-rendered field

Add its key to `OVERRIDDEN_FIELDS` and render it in the template. Key names are the
docker-machine flag with the `fusioncompute-` prefix stripped and camelCased — e.g.
`template-id` → `templateId`, `port-group-urn` → `portGroupUrn`. To populate it from FC,
add a method to `fusioncompute.js` following the `getDatastoreOptions` pattern.

To give a field a different widget without writing a component, set a
`ui-field-hints` annotation on the NodeDriver (read by the `NODE_DRIVER_FIELD_HINTS` block
in `fusioncompute.vue`).
