<script>
// Cloud Credential form for the FusionCompute node driver.
//
// Rancher uses this component instead of the generic key/value form because
// it is registered under cloud-credential/fusioncompute.vue. The VRM
// connection fields are the same ones the machine-config form reads for the
// live dropdowns; skipTlsVerify opts out of the Go driver's API TLS verification.
import CreateEditView from '@shell/mixins/create-edit-view';
import FormValidation from '@shell/mixins/form-validation';
import { Checkbox } from '@components/Form/Checkbox';
import { LabeledInput } from '@components/Form/LabeledInput';

// Credential secrets may surface booleans as strings.
function isTrue(value) {
  return value === true || String(value).toLowerCase() === 'true';
}

export default {
  emits: ['validationChanged', 'valueChanged'],

  components: { Checkbox, LabeledInput },
  mixins:     [CreateEditView, FormValidation],

  data() {
    return {
      fvFormRuleSets: [
        { path: 'decodedData.dcsHostname', rules: ['required'] },
        { path: 'decodedData.dcsUsername', rules: ['required'] },
        { path: 'decodedData.dcsPassword', rules: ['required'] },
        { path: 'decodedData.dcsSiteUri', rules: ['required'] },
      ],
    };
  },

  computed: {
    skipTlsVerify: {
      get() {
        return isTrue(this.value?.decodedData?.skipTlsVerify);
      },
      set(value) {
        this.$emit('valueChanged', 'skipTlsVerify', !!value);
      },
    },
  },

  watch: {
    fvFormIsValid(newValue) {
      this.$emit('validationChanged', !!newValue);
    },
  },
};
</script>

<template>
  <div>
    <div class="row mb-10">
      <div class="col span-8">
        <LabeledInput
          :value="value.decodedData.dcsHostname"
          label="VRM host"
          placeholder="vrm.example.com or vrm.example.com:7443"
          :required="true"
          :mode="mode"
          :rules="fvGetAndReportPathRules('decodedData.dcsHostname')"
          @update:value="$emit('valueChanged', 'dcsHostname', $event)"
        />
      </div>
      <div class="col span-4">
        <LabeledInput
          :value="value.decodedData.dcsPort"
          label="API port"
          placeholder="7443"
          :mode="mode"
          @update:value="$emit('valueChanged', 'dcsPort', $event)"
        />
      </div>
    </div>

    <div class="row mb-10">
      <div class="col span-6">
        <LabeledInput
          :value="value.decodedData.dcsUsername"
          label="Username"
          :required="true"
          :mode="mode"
          :rules="fvGetAndReportPathRules('decodedData.dcsUsername')"
          @update:value="$emit('valueChanged', 'dcsUsername', $event)"
        />
      </div>
      <div class="col span-6">
        <LabeledInput
          :value="value.decodedData.dcsPassword"
          label="Password"
          type="password"
          :required="true"
          :mode="mode"
          :rules="fvGetAndReportPathRules('decodedData.dcsPassword')"
          @update:value="$emit('valueChanged', 'dcsPassword', $event)"
        />
      </div>
    </div>

    <div class="row mb-10">
      <div class="col span-12">
        <LabeledInput
          :value="value.decodedData.dcsSiteUri"
          label="Site URI"
          placeholder="/service/sites/1"
          :required="true"
          :mode="mode"
          :rules="fvGetAndReportPathRules('decodedData.dcsSiteUri')"
          @update:value="$emit('valueChanged', 'dcsSiteUri', $event)"
        />
        <p class="text-muted mt-5">
          Required. Use the FusionCompute site URI, e.g. /service/sites/1. The driver
          does not auto-discover the site.
        </p>
      </div>
    </div>

    <div class="row mb-10">
      <div class="col span-12">
        <Checkbox
          v-model:value="skipTlsVerify"
          :mode="mode"
          label="Skip TLS certificate verification"
        />
        <p class="text-muted mt-5">
          Verification is enabled by default. Check this only for self-signed VRM
          certificates; the driver will then accept any certificate from the VRM. This only
          affects the driver's provisioning API calls; Rancher's meta proxy always verifies
          the certificate for the live Template/Datastore/Cluster/Port Group dropdowns.
        </p>
      </div>
    </div>
  </div>
</template>
