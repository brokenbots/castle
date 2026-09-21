import { StrictMode, Suspense, lazy } from 'react';
import { createRoot } from 'react-dom/client';
import { Provider } from 'react-redux';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';

import { store } from './store';
import { App } from './App';
import { AgentListPage } from './features/agents/AgentListPage';
import { AgentDetailPage } from './features/agents/AgentDetailPage';
import './index.css';

// The runs feature lives in the @castle/run-viewer workspace package and is
// code-split out of the main bundle: the chunk loads when a runs route is
// visited. The same package backs the standalone run-viewer bundle.
const RunListPage = lazy(() =>
  import('@castle/run-viewer').then((m) => ({ default: m.RunListPage })),
);
const RunDetailPage = lazy(() =>
  import('@castle/run-viewer').then((m) => ({ default: m.RunDetailPage })),
);

function RouteFallback() {
  return (
    <div className="flex h-full items-center justify-center text-ink-muted" role="status">
      Loading…
    </div>
  );
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Provider store={store}>
      <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Suspense fallback={<RouteFallback />}>
          <Routes>
            <Route path="/" element={<App />}>
              <Route index element={<Navigate to="/runs" replace />} />
              <Route path="runs" element={<RunListPage />} />
              <Route path="runs/:id" element={<RunDetailPage />} />
              <Route path="agents" element={<AgentListPage />} />
              <Route path="agents/:criteriaId" element={<AgentDetailPage />} />
            </Route>
          </Routes>
        </Suspense>
      </BrowserRouter>
    </Provider>
  </StrictMode>,
);
