// `.shell` is a symlink to node_modules/@rancher/shell, created by
// @rancher/shell/scripts/build-pkg.sh immediately before it invokes
// vue-cli-service. Matches the layout of upstream extension packages:
// https://github.com/rancher/ui-plugin-examples/blob/main/pkg/node-driver/vue.config.js
module.exports = require('./.shell/pkg/vue.config')(__dirname);
