import { Provider } from 'react-redux';
import { HashRouter, Navigate, Route, Routes } from 'react-router-dom';
import { RunListPage } from '../features/runs/RunListPage';
import { RunDetailPage } from '../features/runs/RunDetailPage';
import { NO_CONTROL_CAPABILITIES } from '../features/runs/capabilities';
import type { RunViewerStore } from '../store';

/**
 * The standalone run-viewer shell (CRI-257): a thin static app around the
 * same runs feature parapet serves at /runs/:id, driven by the local data
 * source against the CRI-255 loopback. No auth plumbing (loopback trust
 * domain) and no control RPC until CRI-255 ships, so the control row
 * renders grayed-out with the capability tooltip. A HashRouter keeps the
 * second static root self-contained: any static file server can host it
 * without SPA-rewrite configuration.
 */
export function RunViewerShell({ store }: { store: RunViewerStore }) {
  return (
    <Provider store={store}>
      <HashRouter>
        <Routes>
          <Route path="/" element={<RunListPage />} />
          <Route path="/runs/:id" element={<RunDetailPage capabilities={NO_CONTROL_CAPABILITIES} />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </HashRouter>
    </Provider>
  );
}