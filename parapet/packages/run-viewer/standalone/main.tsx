import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { RunViewerShell } from '../src/standalone/RunViewerShell';
import { createRunViewerStore } from '../src/store';
import { setRunDataSource } from '../src/api/dataSource';
import { localRunDataSource } from '../src/api/localRunDataSource';
// The shared parapet design system (tailwind config + tokens); no forked
// styles for the standalone root.
import '../../../src/index.css';

// Standalone mode: the local data source (CRI-255 loopback) backs the run
// screen; there is no console session and no auth token provider.
setRunDataSource(localRunDataSource);

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <RunViewerShell store={createRunViewerStore()} />
  </StrictMode>,
);