// TypeScript mirror of castle/internal/confighome/record.go (KB-340): the
// per-project workflow config record the project console edits. The Go
// struct tags are the schema of record; these interfaces mirror the field
// surface exactly — ParseRecord rejects unknown fields fail-closed, and the
// record editors must never invent keys the server would reject.
export const API_VERSION = 'criteria.brokenbots.dev/v1';
export const KIND_RECORD = 'ProjectWorkflowConfig';
export const KIND_BINDING = 'CriteriaBinding';
export const KIND_INTERFACE = 'WorkflowInterface';

export const TYPE_IMAGE = 'image';
export const TYPE_URL = 'url';

export const VOLUME_PVC = 'pvc';
export const VOLUME_NFS = 'nfs';
export const VOLUME_TMP = 'tmp';
export const VOLUME_K8S_SECRET = 'k8s-secret';

export const TAG_MATCH_ALL = 'all';
export const TAG_MATCH_ANY = 'any';

export const CUTOVER_DONE = 'done';

/** A route without states triggers on DefaultState (CRI-218). */
export const DEFAULT_STATE = 'Triage';

export const CLASS_DEFAULT = 'dev';
export const CLASS_DEV = 'dev';
export const CLASS_TRIAGE = 'triage';

export const IMAGE_CONFLICT_POLICY_FAIL_CLOSED = 'fail-closed';
export const IMAGE_CONFLICT_POLICY_PREFER_EVENT = 'prefer-event';
export const IMAGE_CONFLICT_POLICY_PREFER_METADATA = 'prefer-metadata';

export const INTERFACE_LOCK_KEY_REPO_URL = 'repoUrl';

/** The only config slots a tree interface may declare (KB-310). */
export const KB310_CONFIG_SLOT_VARIABLES: Record<string, string> = {
  buildCmd: 'build_cmd',
  testCmd: 'test_cmd',
  ciGateCmd: 'ci_gate_cmd',
  baseBranch: 'base_branch',
};

export interface ConfigSlot {
  slot: string;
  variable: string;
  target: string;
  description?: string;
}

export interface Mount {
  name: string;
  mountPath: string;
  kind: string;
}

export interface Interface {
  apiVersion: string;
  kind: string;
  workflow: string;
  description?: string;
  adapters: string[];
  mounts: Mount[];
  secretEnv: string[];
  env: string[];
  configSlots: ConfigSlot[];
  lockKey?: string;
}

export interface Volume {
  name: string;
  kind: string;
  mountPath: string;
  subPath?: string;
  readOnly?: boolean;
  claim?: string;
  server?: string;
  path?: string;
  sizeLimit?: string;
  secretName?: string;
  env?: Record<string, string>;
}

export interface Secret {
  name: string;
  secretProviderClass: string;
  mountPath: string;
  env?: Record<string, string>;
}

export interface Workflow {
  type: string;
  namespace?: string;
  class?: string;
  image?: string;
  url?: string;
  ref?: string;
  volumes?: Volume[];
  secrets?: Secret[];
  env?: Record<string, string>;
  adapterImages?: Record<string, string>;
  imageConflictPolicy?: string;
  interface?: Interface;
}

export interface Route {
  name: string;
  workflow: string;
  project: string;
  tags?: string[];
  tagMatch?: string;
  /** Omitted parses to [DefaultState]; an empty list rejects. */
  states: string[];
  configRef?: string;
}

export interface ConfigEntry {
  buildCmd?: string;
  testCmd?: string;
  ciGateCmd?: string;
  baseBranch?: string;
  tags?: string[];
  tagMatch?: string;
}

export interface Binding {
  apiVersion: string;
  kind: string;
  images: Record<string, string>;
  env: Record<string, string>;
  volumes: Volume[];
  secrets: Secret[];
  config: Record<string, string>;
}

export interface CfgRecord {
  apiVersion: string;
  kind: string;
  registryName: string;
  namespace: string;
  repos?: string[];
  cutover?: string;
  imageConflictPolicy?: string;
  catalog: Record<string, Workflow>;
  routes: Route[];
  configLibrary?: Record<string, ConfigEntry>;
  binding?: Binding;
  digests?: Record<string, string>;
  /** Import bookkeeping; rides the doc but never renders. */
  routesOrdinal?: number;
}

/** The new-record editor's starting shape: the minimum valid record shell. */
export function emptyRecord(registryName: string): CfgRecord {
  return {
    apiVersion: API_VERSION,
    kind: KIND_RECORD,
    registryName,
    namespace: '',
    catalog: {},
    routes: [],
  };
}