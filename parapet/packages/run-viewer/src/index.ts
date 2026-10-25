/**
 * @castle/run-viewer — the parapet runs feature, extracted behind the
 * RunDataSource seam (CRI-257). Hosts import everything from here; relative
 * paths into src/ are not part of the package's public surface.
 */

// Data seam + host extension points.
export type {
  ListRunEventsArgs,
  ResumeArgs,
  RunDataSource,
  RunEventsPage,
  RunStreamArgs,
  RunStreamEnd,
  StopRunArgs,
} from './api/dataSource';
export {
  TERMINAL_EVENT_TYPES,
  getRunDataSource,
  resetRunDataSource,
  setRunDataSource,
} from './api/dataSource';
export { setRunAuthTokenProvider } from './api/client';

// API + generated wire types (re-exported for hosts that need them).
export {
  castleApi,
  mapEnvelope,
  mapRun,
  mapAgent,
  mapRunInspection,
  mapCallerIdentity,
  tsToIso,
  RUNS_PAGE_LIMIT,
  RUN_VIEWER_API_REDUCER_PATH,
  useListRunsQuery,
  useGetRunQuery,
  useInspectRunQuery,
  useListAgentsQuery,
  useGetAgentQuery,
  useGetConnectionStatusQuery,
  useGetCallerIdentityQuery,
  useResumeMutation,
  usePauseRunMutation,
  useStopRunMutation,
  type Run,
  type Agent,
  type RunInspection,
  type InspectRunArgs,
  type EventEnvelope,
  type ListRunsArgs,
  type RunsPage,
  type CallerIdentity,
} from './api/castleApi';
export { connectCodeName, isUnauthenticatedError, classifyError, type PageErrorKind } from './api/errors';
export { castleRunDataSource } from './api/castleDataSource';

// Transport surface for hosts that talk to Castle directly (login probe).
// The console login + decision surface lives on castle.v1.ConsoleService
// (CRI-195/196): criteria.v1 stays an exact mirror of the released SDK.
// The project administration surface (KB-324) rides castle.v1.ProjectService.
// The self-scoped identity read (KB-333) rides castle.v1.IdentityService.
export { server, consoleClient, projectsClient, identityClient, getRuntimeCodec, type Codec } from './api/client';

// Generated castle.v1 wire types, re-exported through the package boundary
// so host slices (e.g. the KB-327 settings console) ride the package surface
// instead of deep-importing package internals.
export type {
  Project as PbProject,
  ProjectMember as PbProjectMember,
  RefirePolicy as PbRefirePolicy,
} from './gen/castle/v1/projects_pb';
export { ProjectMemberKind, ProjectMembershipPolicy } from './gen/castle/v1/projects_pb';

// Shared run-status vocabulary + cells (used by host pages too).
export {
  RUN_TERMINAL_STATUSES,
  RUN_STATUS_TEXT_COLORS,
  RUN_STATUS_CHIP_CLASSES,
  RUN_STATUS_PILL_CLASSES,
  STEP_NODE_STATUS_CLASSES,
  AGENT_STATUS_TEXT_COLORS,
} from './features/runs/runStatus';
export {
  StartedCell,
  DurationCell,
  useDocumentVisible,
  useNow,
} from './features/runs/runCells';

// Store factory.
export {
  createRunViewerStore,
  createUnauthenticatedErrorMiddleware,
  type RunViewerStore,
  type RunViewerStoreOptions,
} from './store';
export type { AppDispatch, RootState } from './store';

// Session-expiry contract: hosts gate their UI on this flag and recover it
// after (re-)login.
export {
  sessionSlice,
  sessionExpired,
  sessionRecovered,
  selectAuthExpired,
  type SessionState,
} from './features/auth/sessionSlice';

// Runs feature UI.
export { RunListPage } from './features/runs/RunListPage';
export { RunDetailPage } from './features/runs/RunDetailPage';
export { runsSlice, selectRunEvents, selectWatchStatus } from './features/runs/runsSlice';
export type { WatchStatus, RunsState } from './features/runs/runsSlice';
export {
  startWatch,
  watchReconnectDelayMs,
  WATCH_RECONNECT_BASE_MS,
  WATCH_RECONNECT_MAX_MS,
  WATCH_RECONNECT_MAX_ATTEMPTS,
} from './features/runs/watchRun';

// Shared components + document title hook.
export { PageHeader } from './components/PageHeader';
export { PageState } from './components/PageState';
export { Breadcrumbs } from './components/Breadcrumbs';
export { DockedPanel } from './components/DockedPanel';
export { DetailPanel } from './components/DetailPanel';
export {
  ColumnVisibilitySelect,
  StickyTableHead,
  makeRowClick,
  useColumnVisibility,
  type TableColumn,
} from './components/DataTable';
export { useDocumentTitle } from './shell/useDocumentTitle';