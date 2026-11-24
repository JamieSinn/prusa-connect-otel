import { defineModule } from '@prusa/connect-ui/slots';

import DeviceSection from './device-section.svelte';
import ExportCard from './export-card.svelte';
import { holdHost } from './host';
import StatusPane from './status-pane.svelte';
import './styles.css';

// Each registration must also be listed under ui.contributes in module.yaml.
export default defineModule((host) => {
  holdHost(host);

  host.register.settings('status', StatusPane);
  host.register.deviceSection('export', DeviceSection);
  host.register.card('export-health', ExportCard);
});
