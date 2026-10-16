import { importTypes } from '@rancher/auto-import';

export default function(plugin) {
  importTypes(plugin);
  plugin.metadata = require('./package.json');
}
