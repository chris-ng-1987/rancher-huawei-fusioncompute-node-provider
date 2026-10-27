<script>
// Machine-config form for the FusionCompute node driver.
//
// Rancher renders either this component OR the shell's generic
// `machine-config/generic.vue` — never both (MachinePool.vue's
// `configComponent` computed). So this component:
//
//   1. resolves the driver schema (same as generic.vue) and hands the
//      remaining fields to <Questions>;
//   2. overrides a few fields with richer widgets:
//        - cpu / memoryMb / diskGb  -> UnitInput (units)
//        - templateId / datastoreUrn / portGroupUrn -> LabeledSelect,
//          populated live from the FusionCompute API via Rancher's
//          meta proxy (see ../fusioncompute.js)
//        - Access -> chooses between an operator-supplied SSH private key
//          (key-only initial access) and the existing username/password
//          bootstrap.
//
// Reference: ui-plugin-examples/pkg/node-driver/machine-config/openstack.vue
// Shell v3 is Vue 3: plain options object, `v-model:value`, no `$set`.
import Loading from '@shell/components/Loading';
import { Banner } from '@components/Banner';
import { Checkbox } from '@components/Form/Checkbox';
import Questions from '@shell/components/Questions';
import UnitInput from '@shell/components/form/UnitInput';
import { LabeledInput } from '@components/Form/LabeledInput';
import LabeledSelect from '@shell/components/form/LabeledSelect';
import CreateEditView from '@shell/mixins/create-edit-view';
import { exceptionToErrorsArray, stringify } from '@shell/utils/error';
import { MANAGEMENT, NORMAN, SECRET } from '@shell/config/types';
import { NODE_DRIVER_FIELD_HINTS } from '@shell/config/labels-annotations';
import { isEmpty } from '@shell/utils/object';
import cloneDeep from 'lodash/cloneDeep';
import FusionCompute from '../fusioncompute.js';

// Fields rendered with our own widgets, so Questions must skip them. The
// access/password fields are grouped under the Access section.
const OVERRIDDEN_FIELDS = [
  'cpu',
  'memoryMb',
  'diskGb',
  'templateId',
  'datastoreUrn',
  'location',
  'portGroupUrn',
  'setVmPassword',
  'generatePassword',
  'sshPassword',
  'sshPrivateKey',
  'sshUser',
];

// Fields intentionally omitted from the form. SSH is always on port 22
// (there is no --fusioncompute-ssh-port flag), and the driver manages its own
// key path (Rancher must not override it), so hide both even if the cached
// schema still advertises them.
const HIDDEN_FIELDS = ['sshPort', 'sshKeyPath'];

// Rancher's auto-generated rke-machine-config CRD types numeric driver
// flags as strings, so these fields must be persisted as strings even
// though UnitInput works with numbers (same as Rancher's built-in drivers,
// e.g. vmwarevsphere.vue's cpuCount/memorySize/diskSize via integerString).
const intString = (field) => ({
  get() {
    const v = this.value?.[field];

    if (v === '' || v === null || v === undefined) {
      return v;
    }
    const n = parseInt(String(v), 10);

    return Number.isFinite(n) ? n : undefined;
  },
  set(v) {
    if (v === '' || v === null || v === undefined) {
      this.value[field] = undefined;

      return;
    }
    const n = parseInt(String(v), 10);

    this.value[field] = Number.isFinite(n) ? String(n) : undefined;
  },
});

// Secret data keys look like `<driverType>-<field>`, e.g.
// fusioncomputecredentialConfig-dcsPassword. Match leniently so a
// different casing/naming doesn't silently break the dropdowns.
function findSecretKey(data, field) {
  const keys = Object.keys(data || {});
  const lower = field.toLowerCase();

  return keys.find((k) => k.toLowerCase().endsWith(`-${ lower }`)) || keys.find((k) => k.toLowerCase().includes(lower));
}

// Secret values are base64. Some code paths hand back already-decoded
// strings, so only decode when the result is printable text.
function decodeValue(raw) {
  if (raw === null || raw === undefined || raw === '') {
    return '';
  }
  const s = String(raw);

  try {
    const d = atob(s);

    if (/^[\t\n\r\x20-\x7E]*$/.test(d)) {
      return d;
    }
  } catch (e) {
    // not base64
  }

  return s;
}

// Fallback: pull a field off the norman CloudCredential object, which may
// expose config fields at the top level or nested under the driver's
// credential config type.
function fieldFromCredential(credential, provider, field) {
  if (!credential) {
    return '';
  }
  const direct = credential[field];

  if (direct !== null && direct !== undefined && direct !== '') {
    return String(direct);
  }
  const nested = credential[`${ provider }credentialConfig`];
  const n = nested && nested[field];

  if (n !== null && n !== undefined && n !== '') {
    return String(n);
  }

  return '';
}

export default {
  emits: ['input'],

  components: {
    Banner, Checkbox, LabeledInput, LabeledSelect, Loading, Questions, UnitInput
  },

  mixins: [CreateEditView],

  props: {
    uuid: {
      type:    String,
      default: '',
    },
    cluster: {
      type:    Object,
      default: () => ({}),
    },
    credentialId: {
      type:    String,
      default: '',
    },
    provider: {
      type:    String,
      default: '',
    },
    disabled: {
      type:    Boolean,
      default: false,
    },
    busy: {
      type:    Boolean,
      default: false,
    },
    poolCreateMode: {
      type:    Boolean,
      default: false,
    },
  },

  async fetch() {
    this.errors = [];
    this.authMode = this.value?.sshPrivateKey ? 'key' : 'password';
    await this.loadSchemaFields();
    await this.loadCloudOptions();
  },

  data() {
    return {
      errors:              null,
      fields:              null,
      cloudCredentialKeys: [],
      // Local UI state: the selected authentication mode must not be derived
      // from sshPrivateKey alone, because the user selects "SSH private key"
      // before typing the key.
      authMode:            'password',
      authOptions:         [
        { label: 'SSH private key', value: 'key' },
        { label: 'Username and password', value: 'password' },
      ],

      // Live FusionCompute lookups
      fc:                  null,
      fcReady:             false,
      listError:           null,
      templateOptions:     [],
      datastoreOptions:    [],
      clusterOptions:      [],
      portGroupOptions:    [],
      loadingTemplates:    false,
      loadingDatastores:   false,
      loadingClusters:     false,
      loadingPortGroups:   false,
    };
  },

  computed: {
    ignoreVariables() {
      return [...(this.cloudCredentialKeys || []), ...OVERRIDDEN_FIELDS, ...HIDDEN_FIELDS];
    },

    cpu:      intString('cpu'),
    memoryMb: intString('memoryMb'),
    diskGb:   intString('diskGb'),
    templateId: {
      get() {
        return this.value?.templateId;
      },
      set(v) {
        this.value.templateId = v;
      },
    },
    datastoreUrn: {
      get() {
        return this.value?.datastoreUrn;
      },
      set(v) {
        this.value.datastoreUrn = v;
      },
    },
    location: {
      get() {
        return this.value?.location;
      },
      set(v) {
        this.value.location = v;
      },
    },
    portGroupUrn: {
      get() {
        return this.value?.portGroupUrn;
      },
      set(v) {
        this.value.portGroupUrn = v;
      },
    },

    setVmPassword: {
      get() {
        return !!this.value?.setVmPassword;
      },
      set(v) {
        this.value.setVmPassword = !!v;
        if (!v) {
          // Nothing to reset, so drop the password choice.
          this.value.generatePassword = false;
          this.value.sshPassword = undefined;
        }
      },
    },
    generatePassword: {
      get() {
        return !!this.value?.generatePassword;
      },
      set(v) {
        this.value.generatePassword = !!v;
        if (v) {
          // The driver generates the value; ignore any typed-in password.
          this.value.sshPassword = undefined;
        }
      },
    },
    authMethod: {
      get() {
        return this.authMode;
      },
      set(v) {
        this.authMode = v;
        if (v === 'key') {
          // Key-only access: the driver must not also attempt a password
          // bootstrap or a FusionCompute password reset.
          this.value.sshPassword = undefined;
          this.value.setVmPassword = false;
          this.value.generatePassword = false;
        } else {
          this.value.sshPrivateKey = undefined;
        }
      },
    },
    sshPrivateKey: {
      get() {
        return this.value?.sshPrivateKey;
      },
      set(v) {
        this.value.sshPrivateKey = v;
      },
    },
    sshPassword: {
      get() {
        return this.value?.sshPassword;
      },
      set(v) {
        this.value.sshPassword = v;
      },
    },
    sshUser: {
      get() {
        return this.value?.sshUser;
      },
      set(v) {
        this.value.sshUser = v;
      },
    },

    selectsDisabled() {
      return this.disabled || this.busy || !this.fcReady;
    },
  },

  watch: {
    'credentialId'() {
      this.$fetch();
    },
    // If a saved config loads its key after the initial fetch (or the field is
    // populated externally), reflect that in the selector. Do not react to the
    // key being cleared: switching back to password mode does that on purpose.
    'value.sshPrivateKey'(v) {
      if (v) {
        this.authMode = 'key';
      }
    },
  },

  methods: {
    stringify,

    // Same resolution as @shell/machine-config/generic.vue.
    async loadSchemaFields() {
      try {
        const fields = await this.$store.getters['plugins/fieldsForDriver'](this.provider);

        this.fields = cloneDeep(fields);
        const name = `rke-machine-config.cattle.io.${ this.provider }config`;

        if (!this.fields) {
          throw new Error(`Machine Driver config schema not found for ${ name }`);
        }
      } catch (e) {
        this.errors = exceptionToErrorsArray(e);

        return;
      }

      try {
        const driver = await this.$store.dispatch('management/find', { type: MANAGEMENT.NODE_DRIVER, id: this.provider });
        const fieldHints = driver?.metadata?.annotations?.[NODE_DRIVER_FIELD_HINTS];
        let parsedHints;

        if (fieldHints) {
          parsedHints = JSON.parse(fieldHints);
        }
        if (parsedHints && !isEmpty(parsedHints)) {
          Object.keys(this.fields).forEach((fieldKey) => {
            if (parsedHints[fieldKey] && parsedHints[fieldKey].type) {
              this.fields[fieldKey].type = parsedHints[fieldKey].type;
            }
          });
        }
      } catch (e) {
        this.errors = exceptionToErrorsArray(e);
      }

      const normanType = this.$store.getters['plugins/credentialFieldForDriver'](this.provider);
      const normanSchema = this.$store.getters['rancher/schemaFor'](`${ normanType }credentialconfig`);

      if (normanSchema) {
        this.cloudCredentialKeys = Object.keys(normanSchema.resourceFields || {});
      } else {
        this.cloudCredentialKeys = await this.$store.getters['plugins/fieldNamesForDriver'](this.provider);
      }
    },

    // Resolve the credential fields the FC API needs. Prefer the k8s
    // Secret — it has stable `<driverType>-<field>` keys and is the only
    // place private fields like the password live. Fall back to the
    // norman CloudCredential object for anything still missing.
    async resolveCredentialFields() {
      const secretId = (this.credentialId || '').replace(':', '/');
      const fields = {
        dcsHostname: '',
        dcsPort:     '',
        dcsUsername: '',
        dcsPassword: '',
        dcsSiteUri:  '',
      };
      let secretKeys = [];
      let secretErr = '';

      try {
        const secret = await this.$store.dispatch('management/find', { type: SECRET, id: secretId });
        const data = secret?.data || {};

        secretKeys = Object.keys(data);
        Object.keys(fields).forEach((f) => {
          const key = findSecretKey(data, f);

          if (key) {
            fields[f] = decodeValue(data[key]);
          }
        });
      } catch (e) {
        secretErr = e?.message || String(e);
      }

      if (['dcsHostname', 'dcsUsername', 'dcsPassword'].some((f) => !fields[f])) {
        try {
          const credential = await this.$store.dispatch('rancher/find', { type: NORMAN.CLOUD_CREDENTIAL, id: this.credentialId });

          Object.keys(fields).forEach((f) => {
            if (!fields[f]) {
              fields[f] = fieldFromCredential(credential, this.provider, f);
            }
          });
        } catch (e) {
          // fall through to the error below
        }
      }

      const missing = ['dcsHostname', 'dcsUsername', 'dcsPassword'].filter((f) => !fields[f]);

      if (missing.length) {
        this.listError = [
          `FusionCompute credential incomplete — missing ${ missing.join(', ') }.`,
          `secret ${ secretId }: ${ secretErr ? `error: ${ secretErr }` : `keys: ${ secretKeys.join(', ') || '(none)' }` }`,
        ].join(' ');

        return null;
      }

      return fields;
    },

    // Read the cloud credential, log in, and pull the option lists.
    async loadCloudOptions() {
      if (!this.credentialId) {
        return;
      }

      const opts = await this.resolveCredentialFields();

      if (!opts) {
        return;
      }

      try {
        this.fc = new FusionCompute(this.$shell?.proxy, opts);
      } catch (e) {
        this.listError = e?.message || String(e);

        return;
      }
      this.fcReady = true;

      this.loadingTemplates = true;
      this.loadingDatastores = true;
      this.loadingClusters = true;
      this.loadingPortGroups = true;

      await Promise.all([
        this.fc.getTemplateOptions()
          .then((o) => {
            this.templateOptions = o;
          })
          .catch((e) => {
            this.listError = e?.message || String(e);
          })
          .finally(() => {
            this.loadingTemplates = false;
          }),
        this.fc.getDatastoreOptions()
          .then((o) => {
            this.datastoreOptions = o;
          })
          .catch((e) => {
            this.listError = e?.message || String(e);
          })
          .finally(() => {
            this.loadingDatastores = false;
          }),
        this.fc.getClusterOptions()
          .then((o) => {
            this.clusterOptions = o;
          })
          .catch((e) => {
            this.listError = e?.message || String(e);
          })
          .finally(() => {
            this.loadingClusters = false;
          }),
        this.fc.getPortGroupOptions()
          .then((o) => {
            this.portGroupOptions = o;
          })
          .catch((e) => {
            this.listError = e?.message || String(e);
          })
          .finally(() => {
            this.loadingPortGroups = false;
          }),
      ]);

      // Keep the already-selected values visible if the list failed.
      this.templateOptions = this.withCurrent(this.templateOptions, this.value?.templateId);
      this.datastoreOptions = this.withCurrent(this.datastoreOptions, this.value?.datastoreUrn);
      this.clusterOptions = this.withCurrent(this.clusterOptions, this.value?.location);
      this.portGroupOptions = this.withCurrent(this.portGroupOptions, this.value?.portGroupUrn);
    },

    // If the stored value isn't in the fetched list (API hiccup / no API),
    // inject it so the select still displays what's configured.
    withCurrent(options, current) {
      if (!current || (options || []).some((o) => o.value === current)) {
        return options;
      }

      return [...(options || []), { label: current, value: current }];
    },
  },
};
</script>

<template>
  <Loading
    v-if="$fetchState.pending"
    :delayed="true"
  />
  <template v-else>
    <div
      v-for="(err, idx) in errors"
      :key="idx"
    >
      <Banner
        color="error"
        :label="stringify(err)"
      />
    </div>

    <Banner
      v-if="listError"
      color="warning"
      :label="`FusionCompute lookup failed: ${ listError }`"
    />

    <div class="row mt-10">
      <div class="col span-12">
        <h3>Machine</h3>
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-6">
        <UnitInput
          v-model:value="cpu"
          :mode="mode"
          :min="1"
          :label="'vCPUs'"
          :suffix="'cores'"
          :disabled="disabled || busy"
        />
      </div>
      <div class="col span-6">
        <UnitInput
          v-model:value="memoryMb"
          :mode="mode"
          :min="1"
          :label="'Memory'"
          :suffix="'MB'"
          :disabled="disabled || busy"
        />
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-6">
        <UnitInput
          v-model:value="diskGb"
          :mode="mode"
          :min="1"
          :label="'System Disk'"
          :suffix="'GB'"
          :disabled="disabled || busy"
        />
      </div>
    </div>

    <div class="row mt-10">
      <div class="col span-12">
        <h3>Placement</h3>
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-6">
        <LabeledSelect
          v-model:value="templateId"
          :mode="mode"
          :label="'Template'"
          :options="templateOptions"
          :loading="loadingTemplates"
          :disabled="selectsDisabled"
          :searchable="true"
        />
      </div>
      <div class="col span-6">
        <LabeledSelect
          v-model:value="datastoreUrn"
          :mode="mode"
          :label="'Datastore'"
          :options="datastoreOptions"
          :loading="loadingDatastores"
          :disabled="selectsDisabled"
          :searchable="true"
        />
      </div>
    </div>

    <div class="row mt-10">
      <div class="col span-6">
        <LabeledSelect
          v-model:value="location"
          :mode="mode"
          :label="'Cluster (Location)'"
          :options="clusterOptions"
          :loading="loadingClusters"
          :disabled="selectsDisabled"
          :searchable="true"
        />
      </div>
    </div>

    <div class="row mt-10">
      <div class="col span-12">
        <h3>Network</h3>
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-6">
        <LabeledSelect
          v-model:value="portGroupUrn"
          :mode="mode"
          :label="'Port Group'"
          :options="portGroupOptions"
          :loading="loadingPortGroups"
          :disabled="selectsDisabled"
          :searchable="true"
        />
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-12">
        <span class="text-muted">The guest obtains its IPv4 address via DHCP.</span>
      </div>
    </div>

    <div class="row mt-10">
      <div class="col span-12">
        <h3>Access</h3>
      </div>
    </div>
    <div class="row mt-10">
      <div class="col span-6">
        <LabeledSelect
          v-model:value="authMethod"
          :mode="mode"
          :label="'Authentication'"
          :options="authOptions"
          :disabled="disabled || busy"
        />
      </div>
    </div>

    <template v-if="authMethod === 'key'">
      <div class="row mt-10">
        <div class="col span-6">
          <LabeledInput
            v-model:value="sshUser"
            :mode="mode"
            :label="'SSH user'"
            :placeholder="'root'"
            :disabled="disabled || busy"
          />
        </div>
      </div>
      <div class="row mt-10">
        <div class="col span-12">
          <LabeledInput
            v-model:value="sshPrivateKey"
            type="multiline-password"
            :mode="mode"
            :label="'SSH private key'"
            :placeholder="'-----BEGIN OPENSSH PRIVATE KEY-----'"
            :disabled="disabled || busy"
          />
        </div>
      </div>
      <div class="row mt-10">
        <div class="col span-12">
          <span class="text-muted">
            Paste an unencrypted private key. The template must already trust the matching public key for the SSH user.
            No password is sent to FusionCompute or used for SSH.
          </span>
        </div>
      </div>
    </template>

    <template v-else>
      <div class="row mt-10">
        <div class="col span-12">
          <Checkbox
            v-model:value="setVmPassword"
            :mode="mode"
            :label="'Reset the guest OS password on first boot (requires FusionCompute guest customization)'"
            :disabled="disabled || busy"
          />
        </div>
      </div>
      <div
        v-if="setVmPassword"
        class="row mt-10"
      >
        <div class="col span-12">
          <Checkbox
            v-model:value="generatePassword"
            :mode="mode"
            :label="'Generate a random password (stored on the Rancher node; no need to type one)'"
            :disabled="disabled || busy"
          />
        </div>
      </div>
      <template v-if="!setVmPassword">
        <div class="row mt-10">
          <div class="col span-6">
            <LabeledInput
              v-model:value="sshUser"
              :mode="mode"
              :label="'SSH user'"
              :placeholder="'root'"
              :disabled="disabled || busy"
            />
          </div>
          <div class="col span-6">
            <LabeledInput
              v-model:value="sshPassword"
              type="password"
              :mode="mode"
              :label="'Existing guest password (used once to install the key; leave blank if the template already trusts the key)'"
              :disabled="disabled || busy"
            />
          </div>
        </div>
      </template>
      <template v-else>
        <div
          v-if="!generatePassword"
          class="row mt-10"
        >
          <div class="col span-6">
            <LabeledInput
              v-model:value="sshPassword"
              type="password"
              :mode="mode"
              :label="'New guest password (reset on first boot)'"
              :disabled="disabled || busy"
            />
          </div>
        </div>
        <div class="row mt-10">
          <div class="col span-12">
            <span class="text-muted">FusionCompute resets the password for the <strong>root</strong> account (the reset is not permitted for other users); the driver logs in as root to install the SSH key. The template must allow root password SSH.</span>
          </div>
        </div>
      </template>
    </template>

    <template v-if="fields">
      <div class="row mt-10">
        <div class="col span-12">
          <h3>Advanced</h3>
        </div>
      </div>
      <Questions
        :value="value"
        :mode="mode"
        :tabbed="false"
        :source="fields"
        :ignore-variables="ignoreVariables"
        :target-namespace="value.metadata.namespace"
        :disabled="disabled"
        @update:value="$emit('input', $event)"
      />
    </template>
  </template>
</template>
