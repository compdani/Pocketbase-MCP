// Svelte entry point.
//
// Importing agent-panel's page entry runs its side effect: it defines the
// <agent-page> custom element. It must happen before Svelte draws the element
// (see agent-panel/AGENTS.md §8), so it is the first import here.
import 'agent-panel/page';

import { mount } from 'svelte';
import App from './App.svelte';
import './styles.css';

export default mount(App, { target: document.getElementById('app') });
