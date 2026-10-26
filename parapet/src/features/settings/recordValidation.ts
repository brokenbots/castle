// TypeScript mirror of castle/internal/confighome/validate.go (KB-340).
// Castle validates SHAPE only ("dave 10-23 rule") — field names, closed
// vocabularies, cross-references — and these functions mirror those rules
// rule-for-rule so the console's live validation previews the exact result
// the server-side store validation would produce (fail-fast UI over the
// same store validation). Wording follows the Go error messages.
import {
  API_VERSION,
  CLASS_DEV,
  CLASS_TRIAGE,
  CUTOVER_DONE,
  DEFAULT_STATE,
  IMAGE_CONFLICT_POLICY_FAIL_CLOSED,
  IMAGE_CONFLICT_POLICY_PREFER_EVENT,
  IMAGE_CONFLICT_POLICY_PREFER_METADATA,
  INTERFACE_LOCK_KEY_REPO_URL,
  KB310_CONFIG_SLOT_VARIABLES,
  KIND_BINDING,
  KIND_INTERFACE,
  KIND_RECORD,
  TAG_MATCH_ALL,
  TAG_MATCH_ANY,
  TYPE_IMAGE,
  TYPE_URL,
  VOLUME_K8S_SECRET,
  VOLUME_NFS,
  VOLUME_PVC,
  VOLUME_TMP,
  type Binding,
  type ConfigEntry,
  type Interface,
  type CfgRecord,
  type Route,
  type Secret,
  type Volume,
  type Workflow,
} from './record';

// Same regexes as castle/internal/confighome/validate.go.
export const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
export const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
export const SECRET_KEY_RE = /^[A-Za-z0-9._-]+$/;
export const REPO_REF_RE = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/;
export const WHITESPACE_RE = /\s/;

export const INTERFACE_NAME_RE = /^[A-Za-z][A-Za-z0-9_-]*$/;
// Trailing slash rejected to match scripts/interface_validate.mjs
// (byte-exact mount paths, no trailing-slash normalization).
export const INTERFACE_PATH_RE = /^\/.+[^/]$/;
export const INTERFACE_ENV_RE = /^[A-Z][A-Z0-9_]*$/;
export const INTERFACE_VAR_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;
export const INTERFACE_SLOT_RE = /^[a-z][a-zA-Z0-9_]*$/;

/** DNS-1123 label: null when valid, a message otherwise. */
export function validateLabel(name: string, what: string): string | null {
  if (!LABEL_RE.test(name)) return `${what} "${name}" is not a DNS-1123 label`;
  return null;
}

type StrMap = { [k: string]: string };
type ObjMap = { [k: string]: unknown };

function envMapErrors(env: StrMap | undefined, where: string): string[] {
  const out: string[] = [];
  for (const name of Object.keys(env ?? {})) {
    if (!ENV_NAME_RE.test(name)) {
      out.push(`${where} has invalid env var name "${name}"`);
    }
    if ((env![name] ?? '') === '') {
      out.push(`${where}.${name} must be a string value`);
    }
  }
  return out;
}

function secretEnvErrors(env: StrMap | undefined): string[] {
  const out: string[] = [];
  for (const name of Object.keys(env ?? {})) {
    if (!ENV_NAME_RE.test(name)) {
      out.push(`env has invalid env var name "${name}"`);
    }
    if (!SECRET_KEY_RE.test(env![name] ?? '')) {
      out.push(`env.${name} value "${env![name]}" is not a valid key name`);
    }
  }
  return out;
}

/** Mirror confighome.validateAdapterImages. */
export function validateAdapterImages(images: StrMap | undefined): string[] {
  const out: string[] = [];
  for (const kind of Object.keys(images ?? {})) {
    if (!LABEL_RE.test(kind)) {
      out.push(`adapterImages has invalid adapter kind "${kind}"`);
    }
    const ref = images![kind] ?? '';
    if (ref === '' || WHITESPACE_RE.test(ref)) {
      out.push(`adapterImages.${kind} must be a non-empty image reference without whitespace`);
    }
  }
  return out;
}

/**
 * Mirror confighome.validateVolume: pvc/nfs/tmp/k8s-secret shapes with
 * fail-closed sibling-kind exclusivity.
 */
export function validateVolume(v: Volume): string[] {
  const out: string[] = [];
  const labelErr = validateLabel(v.name, 'name');
  if (labelErr) out.push(labelErr);
  if (!(v.mountPath ?? '').startsWith('/')) {
    out.push(`mountPath "${v.mountPath}" must be absolute`);
  }
  out.push(...envMapErrors(v.env, 'env'));
  switch (v.kind) {
    case VOLUME_PVC:
      if ((v.claim ?? '') === '') out.push('kind=pvc requires a claim');
      if ((v.server ?? '') !== '' || (v.path ?? '') !== '' || (v.sizeLimit ?? '') !== '' || (v.secretName ?? '') !== '') {
        out.push('kind=pvc must not declare server, path, sizeLimit, or secretName');
      }
      break;
    case VOLUME_NFS:
      if ((v.server ?? '') === '' || (v.path ?? '') === '') out.push('kind=nfs requires server and path');
      if ((v.claim ?? '') !== '' || (v.sizeLimit ?? '') !== '' || (v.secretName ?? '') !== '') {
        out.push('kind=nfs must not declare claim, sizeLimit, or secretName');
      }
      break;
    case VOLUME_TMP:
      if ((v.claim ?? '') !== '' || (v.server ?? '') !== '' || (v.path ?? '') !== '' || (v.secretName ?? '') !== '') {
        out.push('kind=tmp must not declare claim, server, path, or secretName');
      }
      break;
    case VOLUME_K8S_SECRET:
      if ((v.secretName ?? '') === '') out.push('kind=k8s-secret requires a secretName');
      if ((v.claim ?? '') !== '' || (v.server ?? '') !== '' || (v.path ?? '') !== '' || (v.sizeLimit ?? '') !== '') {
        out.push('kind=k8s-secret must not declare claim, server, path, or sizeLimit');
      }
      break;
    default:
      out.push(`kind must be "${VOLUME_PVC}", "${VOLUME_NFS}", "${VOLUME_TMP}", or "${VOLUME_K8S_SECRET}", got "${v.kind ?? ''}"`);
  }
  return out;
}

/** Mirror confighome.validateSecret: name-only references (CRI-214 3.8). */
export function validateSecret(s: Secret): string[] {
  const out: string[] = [];
  const labelErr = validateLabel(s.name, 'name');
  if (labelErr) out.push(labelErr);
  if ((s.secretProviderClass ?? '') === '') out.push('secretProviderClass is required');
  if (!(s.mountPath ?? '').startsWith('/')) out.push(`mountPath "${s.mountPath}" must be absolute`);
  out.push(...secretEnvErrors(s.env));
  return out;
}

/** Mirror the per-workflow shape validation including ADR-0005 D1/D2 modes. */
export function validateWorkflowShape(name: string, wf: Workflow): string[] {
  const out: string[] = [];
  const labelErr = validateLabel(name, 'name');
  if (labelErr) out.push(labelErr);
  switch (wf.type) {
    case TYPE_IMAGE:
      if ((wf.image ?? '') === '') out.push('type=image requires an image');
      if ((wf.url ?? '') !== '' || (wf.ref ?? '') !== '') {
        out.push('type=image must not declare url or ref');
      }
      if (wf.interface !== undefined) out.push('type=image must not declare interface');
      break;
    case TYPE_URL:
      if ((wf.url ?? '') === '') out.push('type=url requires a url');
      if (wf.interface === undefined) out.push('type=url requires interface (KB-310)');
      else out.push(...interfaceErrors(wf.interface));
      break;
    default:
      out.push(`type must be "${TYPE_IMAGE}" or "${TYPE_URL}", got "${wf.type ?? ''}"`);
  }
  if ((wf.class ?? '') !== '' && wf.class !== CLASS_DEV && wf.class !== CLASS_TRIAGE) {
    out.push(`class must be "${CLASS_DEV}" or "${CLASS_TRIAGE}", got "${wf.class}"`);
  }
  if (wf.imageConflictPolicy !== undefined && wf.imageConflictPolicy !== '') {
    if (wf.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_FAIL_CLOSED &&
        wf.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_PREFER_EVENT &&
        wf.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_PREFER_METADATA) {
      out.push(
        `imageConflictPolicy must be "${IMAGE_CONFLICT_POLICY_FAIL_CLOSED}", "${IMAGE_CONFLICT_POLICY_PREFER_EVENT}" or "${IMAGE_CONFLICT_POLICY_PREFER_METADATA}", got "${wf.imageConflictPolicy}"`,
      );
    }
  }
  if ((wf.namespace ?? '') !== '' && !LABEL_RE.test(wf.namespace!)) {
    out.push(`namespace "${wf.namespace}" is not a DNS-1123 label`);
  }
  out.push(...envMapErrors(wf.env, 'env'));
  out.push(...validateAdapterImages(wf.adapterImages));

  const seenV: string[] = [];
  (wf.volumes ?? []).forEach((v, i) => {
    out.push(...validateVolume(v).map((e) => `volumes[${i}] (${v.name}): ${e}`));
    if (seenV.includes(v.name)) out.push(`volumes[${i}] (${v.name}): name is not unique within the workflow`);
    seenV.push(v.name);
  });
  const seenS: string[] = [];
  (wf.secrets ?? []).forEach((s, i) => {
    out.push(...validateSecret(s).map((e) => `secrets[${i}] (${s.name}): ${e}`));
    if (seenS.includes(s.name)) out.push(`secrets[${i}] (${s.name}): name is not unique within the workflow`);
    seenS.push(s.name);
  });
  return out;
}

/** Mirror confighome.validateInterface (KB-310). */
export function interfaceErrors(iface: Interface): string[] {
  const out: string[] = [];
  if (iface.apiVersion !== API_VERSION) {
    out.push(`apiVersion must be "${API_VERSION}", got "${iface.apiVersion ?? ''}"`);
  }
  if (iface.kind !== KIND_INTERFACE) {
    out.push(`kind must be "${KIND_INTERFACE}", got "${iface.kind ?? ''}"`);
  }
  if ((iface.workflow ?? '') === '' || !INTERFACE_NAME_RE.test(iface.workflow)) {
    out.push(`workflow ("${iface.workflow ?? ''}") is not a tree name`);
  }
  const adapterSeen = new Set<string>();
  for (const kind of iface.adapters ?? []) {
    if (!INTERFACE_NAME_RE.test(kind)) out.push(`adapter kind "${kind}" is not a valid kind name`);
    if (adapterSeen.has(kind)) out.push('adapter kinds are not unique');
    adapterSeen.add(kind);
  }
  const seenMounts = new Set<string>();
  (iface.mounts ?? []).forEach((m, i) => {
    if (!INTERFACE_NAME_RE.test(m.name)) out.push(`mounts[${i}]: name "${m.name}" is not a mount name`);
    if (seenMounts.has(m.name)) out.push(`mounts[${i}] (${m.name}): name is not unique`);
    seenMounts.add(m.name);
    if (!INTERFACE_PATH_RE.test(m.mountPath ?? '')) {
      out.push(`mounts[${i}] (${m.name}): mountPath "${m.mountPath}" must be an absolute path without trailing slash`);
    }
    if (m.kind !== VOLUME_PVC && m.kind !== VOLUME_TMP) {
      out.push(`mounts[${i}] (${m.name}): kind "${m.kind ?? ''}" must be pvc or tmp`);
    }
  });
  const seenSecretEnv = new Set<string>();
  for (const name of iface.secretEnv ?? []) {
    if (seenSecretEnv.has(name)) out.push('secretEnv lanes are not unique');
    seenSecretEnv.add(name);
    if (!INTERFACE_ENV_RE.test(name)) out.push(`secretEnv "${name}" is not an env name (UPPER_SNAKE)`);
  }
  const seenEnv = new Set<string>();
  for (const name of iface.env ?? []) {
    if (seenEnv.has(name)) out.push('env names are not unique');
    seenEnv.add(name);
    if (!INTERFACE_ENV_RE.test(name)) out.push(`env "${name}" is not an env name (UPPER_SNAKE)`);
  }
  const seenSlots = new Set<string>();
  (iface.configSlots ?? []).forEach((s, i) => {
    if (!INTERFACE_SLOT_RE.test(s.slot ?? '')) {
      out.push(`configSlots[${i}]: slot "${s.slot ?? ''}" is not a slot name (lowercase-first)`);
    }
    if (seenSlots.has(s.slot)) out.push(`configSlots[${i}] (${s.slot}): slot is not unique`);
    seenSlots.add(s.slot);
    const wantVar = KB310_CONFIG_SLOT_VARIABLES[s.slot ?? ''];
    if (wantVar === undefined) {
      out.push(
        `configSlots[${i}] (${s.slot}): slot is not a platform config key (declare only buildCmd, testCmd, ciGateCmd, or baseBranch — the keys a project binding can carry, KB-310)`,
      );
    }
    if (s.variable !== wantVar) {
      out.push(
        `configSlots[${i}] (${s.slot}): variable "${s.variable ?? ''}" is not the slot's delivery variable "${wantVar}" (the KB-103 render delivers the value as the tree variable, KB-310)`,
      );
    }
    if (!INTERFACE_VAR_RE.test(s.variable ?? '')) {
      out.push(`configSlots[${i}] (${s.slot}): variable "${s.variable ?? ''}" is not a tree variable`);
    }
    if ((s.target ?? '') === '') out.push(`configSlots[${i}] (${s.slot}): target is required`);
  });
  if (iface.lockKey !== undefined && iface.lockKey !== '' && iface.lockKey !== INTERFACE_LOCK_KEY_REPO_URL) {
    out.push(`lockKey must be "${INTERFACE_LOCK_KEY_REPO_URL}" or absent, got "${iface.lockKey}"`);
  }
  return out;
}

const CONFIG_ENTRY_FIELDS = ['buildCmd', 'testCmd', 'ciGateCmd', 'baseBranch'] as const;

function isBlank(value: string | undefined): boolean {
  if (value === undefined || value === '') return false; // absent: skip
  return value.trim() === '';
}

/** Mirror confighome.validateConfigEntry (KB-103, KB-154). */
export function validateConfigEntryShape(name: string, entry: ConfigEntry): string[] {
  const out: string[] = [];
  const labelErr = validateLabel(name, 'name');
  if (labelErr) out.push(labelErr);
  for (const field of CONFIG_ENTRY_FIELDS) {
    const value = (entry[field] as string | undefined) ?? '';
    if (!isBlank(value)) continue;
    if (value === '') continue;
    out.push(`${field} must not be blank when declared`);
  }
  if (WHITESPACE_RE.test(entry.baseBranch ?? '')) {
    out.push(`baseBranch "${entry.baseBranch}" must be a git ref name without whitespace`);
  }
  for (const tag of entry.tags ?? []) {
    if (tag === '') out.push('tags must not contain empty names');
  }
  if (entry.tagMatch !== undefined && entry.tagMatch !== '' && entry.tagMatch !== TAG_MATCH_ALL && entry.tagMatch !== TAG_MATCH_ANY) {
    out.push(`tagMatch must be "${TAG_MATCH_ALL}" or "${TAG_MATCH_ANY}", got "${entry.tagMatch}"`);
  }
  return out;
}

function isHex64(s: string): boolean {
  if (s.length !== 64) return false;
  for (const c of s) {
    if (!('0123456789abcdef'.includes(c))) return false;
  }
  return true;
}

/** Mirror the binding half of ValidateBinding (KB-311, ADR 307). */
export function bindingShapeErrors(b: {
  images?: StrMap;
  env?: StrMap;
  volumes?: Volume[];
  secrets?: Secret[];
  config?: StrMap;
}): string[] {
  const out: string[] = [];
  out.push(...validateAdapterImages(b.images).map((e) => `binding images: ${e}`));
  out.push(...envMapErrors(b.env, 'binding env'));
  const seen: Set<string> = new Set();
  (b.volumes ?? []).forEach((v, i) => {
    if (seen.has(v.name)) out.push(`binding volumes[${i}] duplicates name "${v.name}"`);
    seen.add(v.name);
    out.push(...validateVolume(v).map((e) => `binding volume "${v.name}": ${e}`));
  });
  (b.secrets ?? []).forEach((s, i) => {
    if (seen.has(s.name)) out.push(`binding secrets[${i}] duplicates name "${s.name}"`);
    seen.add(s.name);
    out.push(...validateSecret(s).map((e) => `binding secret "${s.name}": ${e}`));
  });
  for (const slot of Object.keys(b.config ?? {})) {
    if (KB310_CONFIG_SLOT_VARIABLES[slot] === undefined) {
      out.push(
        `binding config slot "${slot}" is not one of the platform config keys (buildCmd, testCmd, ciGateCmd, baseBranch)`,
      );
    }
    if ((b.config![slot] ?? '') === '') {
      out.push(`binding config.${slot} must be a string value`);
    }
  }
  return out;
}

/**
 * Full ValidateBinding: apiVersion/kind plus the shape rules.
 */
export function bindingErrors(b: Binding): string[] {
  const out: string[] = [];
  if (b.apiVersion !== API_VERSION) {
    out.push(`binding apiVersion must be "${API_VERSION}", got "${b.apiVersion ?? ''}"`);
  }
  if (b.kind !== KIND_BINDING) {
    out.push(`binding kind must be "${KIND_BINDING}", got "${b.kind ?? ''}"`);
  }
  out.push(...bindingShapeErrors(b));
  return out;
}

/**
 * Mirror the route/config cross-reference (KB-339 item 3): config keys the
 * tree's DECLARED interface does not declare reject.
 */
function routeConfigCross(routeName: string, workflowName: string, wf: Workflow | undefined, configRef: string, entry: ConfigEntry | undefined): string[] {
  if (!wf?.interface) return [];
  const declared = new Set((wf.interface.configSlots ?? []).map((s) => s.slot));
  const out: string[] = [];
  for (const field of CONFIG_ENTRY_FIELDS) {
    if (((entry?.[field] as string | undefined) ?? '') === '') continue;
    if (!declared.has(field)) {
      out.push(
        `configRef "${configRef}": route "${routeName}" workflow "${workflowName}" interface does not declare the config slot "${field}"`,
      );
    }
  }
  return out;
}

/**
 * Mirror confighome Record.Validate(): every shape rule the server's
 * SetProjectConfig enforces, in record order. The array returns ALL errors
 * so the editor's live preview shows the full surface; the first entry
 * mirrors what the server would reject first.
 */
export function validateRecord(r: CfgRecord): string[] {
  const out: string[] = [];
  if (r.apiVersion !== API_VERSION) {
    out.push(`apiVersion must be "${API_VERSION}", got "${r.apiVersion ?? ''}"`);
  }
  if (r.kind !== KIND_RECORD) out.push(`kind must be "${KIND_RECORD}", got "${r.kind ?? ''}"`);
  if ((r.registryName ?? '').trim() === '') {
    out.push('registryName required: the record is the project\'s registry entry');
  }
  if (!LABEL_RE.test(r.namespace ?? '')) {
    out.push(`namespace "${r.namespace ?? ''}" is not a DNS-1123 label`);
  }
  for (const repo of r.repos ?? []) {
    if (!REPO_REF_RE.test(repo)) out.push(`repo "${repo}" is not an "owner/name" reference`);
  }
  if (r.cutover !== undefined && r.cutover !== '' && r.cutover !== CUTOVER_DONE) {
    out.push(`cutover must be "${CUTOVER_DONE}" or absent, got "${r.cutover}" (binding-first, deletion second)`);
  }
  if (r.imageConflictPolicy !== undefined && r.imageConflictPolicy !== '') {
    if (r.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_FAIL_CLOSED &&
        r.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_PREFER_EVENT &&
        r.imageConflictPolicy !== IMAGE_CONFLICT_POLICY_PREFER_METADATA) {
      out.push(
        `imageConflictPolicy must be "${IMAGE_CONFLICT_POLICY_FAIL_CLOSED}", "${IMAGE_CONFLICT_POLICY_PREFER_EVENT}" or "${IMAGE_CONFLICT_POLICY_PREFER_METADATA}", got "${r.imageConflictPolicy}"`,
      );
    }
  }
  for (const name of Object.keys(r.catalog ?? {}).sort()) {
    out.push(...validateWorkflowShape(name, r.catalog![name]).map((e) => `catalog["${name}"]: ${e}`));
  }
  for (const name of Object.keys(r.configLibrary ?? {}).sort()) {
    out.push(...validateConfigEntryShape(name, r.configLibrary![name]).map((e) => `configLibrary["${name}"]: ${e}`));
  }
  const seenRoutes = new Set<string>();
  (r.routes ?? []).forEach((rt, i) => {
    if (!LABEL_RE.test(rt.name ?? '')) {
      out.push(`routes[${i}].name "${rt.name ?? ''}" is not a DNS-1123 label`);
    }
    if (seenRoutes.has(rt.name)) {
      out.push(`routes[${i}].name "${rt.name}" is not unique`);
    }
    seenRoutes.add(rt.name);
    if ((rt.project ?? '') !== (r.registryName ?? '')) {
      out.push(
        `routes[${i}] (${rt.name}): project "${rt.project}" must be the record's registryName "${r.registryName}" (a record carries only its own routes)`,
      );
    }
    if (!(rt.workflow in (r.catalog ?? {}))) {
      out.push(`routes[${i}] (${rt.name}): workflow "${rt.workflow}" is not present in the record's catalog`);
    }
    if ((rt.configRef ?? '') !== '' && !((rt.configRef ?? '') in (r.configLibrary ?? {}))) {
      out.push(`routes[${i}] (${rt.name}): configRef "${rt.configRef}" is not present in the record's configLibrary`);
    }
    if ((rt.states ?? []).length === 0) {
      out.push(
        `routes[${i}] (${rt.name}): states must not be empty (records store states materialized: the payload default is [${DEFAULT_STATE}])`,
      );
    }
    for (const s of rt.states ?? []) {
      if (s === '') out.push(`routes[${i}] (${rt.name}): states must not contain empty names`);
    }
    for (const tag of rt.tags ?? []) {
      if (tag === '') out.push(`routes[${i}] (${rt.name}): tags must not contain empty names`);
    }
    if (rt.tagMatch !== undefined && rt.tagMatch !== '' && rt.tagMatch !== TAG_MATCH_ALL && rt.tagMatch !== TAG_MATCH_ANY) {
      out.push(`routes[${i}] (${rt.name}): tagMatch must be "${TAG_MATCH_ALL}" or "${TAG_MATCH_ANY}", got "${rt.tagMatch}"`);
    }
  });
  if (r.binding !== undefined) {
    out.push(...bindingErrors(r.binding).map((e) => `binding: ${e}`));
  }
  for (const name of Object.keys(r.digests ?? {}).sort()) {
    if (!(name in (r.catalog ?? {}))) {
      out.push(`digests["${name}"]: no catalog entry named "${name}"`);
    }
    if (!isHex64(r.digests![name] ?? '')) {
      out.push(`digests["${name}"]: "${r.digests![name]}" is not a sha256 hex digest`);
    }
  }
  (r.routes ?? []).forEach((rt, i) => {
    if ((rt.configRef ?? '') === '') return;
    out.push(
      ...routeConfigCross(rt.name, rt.workflow, r.catalog?.[rt.workflow], rt.configRef!, r.configLibrary?.[rt.configRef!]).map(
        (e) => `routes[${i}] (${rt.name}): ${e}`,
      ),
    );
  });
  return out;
}

// --- Unknown-field rejection (mirror ParseRecord.DisallowUnknownFields) ---

const RECORD_FIELDS = new Set([
  'apiVersion', 'kind', 'registryName', 'namespace', 'repos', 'cutover',
  'imageConflictPolicy', 'catalog', 'routes', 'configLibrary', 'binding',
  'digests', 'routesOrdinal',
]);
const WORKFLOW_FIELDS = new Set([
  'type', 'namespace', 'class', 'image', 'url', 'ref', 'volumes', 'secrets',
  'env', 'adapterImages', 'imageConflictPolicy', 'interface',
]);
const INTERFACE_FIELDS = new Set([
  'apiVersion', 'kind', 'workflow', 'description', 'adapters', 'mounts',
  'secretEnv', 'env', 'configSlots', 'lockKey',
]);
const MOUNT_FIELDS = new Set(['name', 'mountPath', 'kind']);
const CONFIG_SLOT_FIELDS = new Set(['slot', 'variable', 'target', 'description']);
const VOLUME_FIELDS = new Set([
  'name', 'kind', 'mountPath', 'subPath', 'readOnly', 'claim', 'server',
  'path', 'sizeLimit', 'secretName', 'env',
]);
const SECRET_FIELDS = new Set(['name', 'secretProviderClass', 'mountPath', 'env']);
const ROUTE_FIELDS = new Set(['name', 'workflow', 'project', 'tags', 'tagMatch', 'states', 'configRef']);
const CONFIG_ENTRY_FIELDS_SET = new Set(['buildCmd', 'testCmd', 'ciGateCmd', 'baseBranch', 'tags', 'tagMatch']);
const BINDING_FIELDS = new Set(['apiVersion', 'kind', 'images', 'env', 'volumes', 'secrets', 'config']);

function objectAt(v: unknown): ObjMap | undefined {
  return typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as ObjMap) : undefined;
}

function checkKeys(v: unknown, allowed: Set<string>, path: string, out: string[]): void {
  const obj = objectAt(v);
  if (obj === undefined) return;
  for (const key of Object.keys(obj)) {
    if (!allowed.has(key)) out.push(`${path}: unknown field "${key}"`);
  }
}

function checkVolumes(v: unknown, path: string, out: string[]): void {
  if (!Array.isArray(v)) return;
  for (let i = 0; i < v.length; i++) checkKeys(v[i], VOLUME_FIELDS, `${path}[${i}]`, out);
}

function checkSecrets(v: unknown, path: string, out: string[]): void {
  if (!Array.isArray(v)) return;
  for (let i = 0; i < v.length; i++) checkKeys(v[i], SECRET_FIELDS, `${path}[${i}]`, out);
}

function checkInterface(v: unknown, path: string, out: string[]): void {
  const obj = objectAt(v);
  if (obj === undefined) return;
  checkKeys(obj, INTERFACE_FIELDS, path, out);
  if (Array.isArray(obj.mounts)) {
    obj.mounts.forEach((m, i) => checkKeys(m, MOUNT_FIELDS, `${path}.mounts[${i}]`, out));
  }
  if (Array.isArray(obj.configSlots)) {
    obj.configSlots.forEach((s, i) => checkKeys(s, CONFIG_SLOT_FIELDS, `${path}.configSlots[${i}]`, out));
  }
}

/**
 * Mirror ParseRecord's fail-closed unknown-field rejection over the parsed
 * JSON. Returns the list of unknown-field violations (empty = clean).
 *
 * Note the Go tag surface distinguishes `states` (no omitempty) from
 * optional fields only so unknown keys at every level reject exactly like
 * the decoder does.
 */
export function unknownFieldErrors(json: unknown): string[] {
  const out: string[] = [];
  const rec = objectAt(json);
  if (rec === undefined) return out;
  checkKeys(rec, RECORD_FIELDS, 'record', out);
  const catalog = objectAt(rec.catalog);
  if (catalog) {
    for (const name of Object.keys(catalog)) {
      const wf = objectAt(catalog[name]);
      if (wf === undefined) continue;
      checkKeys(wf, WORKFLOW_FIELDS, `catalog["${name}"]`, out);
      if (wf.interface !== undefined) checkInterface(wf.interface, `catalog["${name}"].interface`, out);
      checkVolumes(wf.volumes, `catalog["${name}"].volumes`, out);
      checkSecrets(wf.secrets, `catalog["${name}"].secrets`, out);
    }
  }
  const lib = objectAt(rec.configLibrary);
  if (lib) {
    for (const name of Object.keys(lib)) {
      checkKeys(lib[name], CONFIG_ENTRY_FIELDS_SET, `configLibrary["${name}"]`, out);
    }
  }
  if (Array.isArray(rec.routes)) rec.routes.forEach((rt, i) => checkKeys(rt, ROUTE_FIELDS, `routes[${i}]`, out));
  if (rec.binding !== undefined) {
    const bnd = objectAt(rec.binding);
    if (bnd) {
      checkKeys(bnd, BINDING_FIELDS, 'binding', out);
      checkVolumes(bnd.volumes, 'binding.volumes', out);
      checkSecrets(bnd.secrets, 'binding.secrets', out);
    }
  }
  return out;
}

/** Default route shape the routes editor inserts (the payload default). */
export function newRoute(registryName: string): Route {
  return { name: '', workflow: '', project: registryName, states: [DEFAULT_STATE] };
}