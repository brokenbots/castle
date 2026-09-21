import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Provider } from 'react-redux';
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom';

import { store } from './store';
import { App } from './App';
import { RunListPage } from '@castle/run-viewer';
import { RunDetailPage } from '@castle/run-viewer';
import { AgentListPage } from './features/agents/AgentListPage';
import { AgentDetailPage } from './features/agents/AgentDetailPage';

import './index.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Provider store={store}>
      <BrowserRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <Routes>
          <Route path="/" element={<App />}>
            <Route index element={<Navigate to="/runs" replace />} />
            <Route path="runs" element={<RunListPage />} />
            <Route path="runs/:id" element={<RunDetailPage />} />
            <Route path="agents" element={<AgentListPage />} />
            <Route path="agents/:criteriaId" element={<AgentDetailPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </Provider>
  </StrictMode>,
);
