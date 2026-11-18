// Vue entry point.
//
// Importing agent-panel's page entry runs its side effect: it defines the
// <agent-page> custom element. It must happen before Vue draws the element
// (see agent-panel/AGENTS.md §8), so it is the first import here rather than
// something a component does lazily.
import 'agent-panel/page';

import { createApp } from 'vue';
import App from './App.vue';
import './styles.css';

createApp(App).mount('#app');
