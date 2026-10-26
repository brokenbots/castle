// Parity tests for the record-validation mirror (KB-340, live validation).
// The server-side rules live in castle/internal/confighome/validate.go —
// the mirror here must reject exactly what the server rejects, so each rule
// carries its negative and its clean case. Wording is not pinned;
// rejections are.
import { describe, expect, test } from 'vitest';
import {
  API_VERSION,
  DEFAULT_STATE,
  type Binding,
  type CfgRecord,
  type Interface,
  type Workflow,
  emptyRecord,
} from './record';
import {
  bindingErrors,
  interfaceErrors,
  newRoute,
  unknownFieldErrors,
  validateConfigEntryShape,
  validateRecord,
  validateVolume,
  validateWorkflowShape,
} from './recordValidation';

// A minimal record the server accepts: fixed apiVersion/kind, a label
// namespace, one image workflow, one route on the default state.
function minimalRecord(): CfgRecord {
  const rec = emptyRecord('squad');
  rec.namespace = 'squad-ns';
  rec.catalog['ship-it'] = { type: 'image', image: 'registry.example/castle/ship@sha256:abc' };
  rec.routes.push({
    name: 'triage-runs',
    workflow: 'ship-it',
    project: 'squad',
    states: [DEFAULT_STATE],
  });
  return rec;
}

describe('validateRecord (mirror of confighome.Record.Validate)', () => {
  test('a minimal record validates clean', () => {
    expect(validateRecord(minimalRecord())).toEqual([]);
  });

  test('apiVersion and kind are fixed', () => {
    const rec = minimalRecord();
    rec.apiVersion = 'v2';
    rec.kind = 'Other';
    const errs = validateRecord(rec);
    expect(errs.join('\n')).toMatch(/apiVersion must be "criteria\.brokenbots\.dev\/v1"/);
    expect(errs.join('\n')).toMatch(/kind must be "ProjectWorkflowConfig"/);
  });

  test('registryName is required (the record is the registry entry)', () => {
    const rec = minimalRecord();
    rec.registryName = '   ';
    expect(validateRecord(rec).join('\n')).toMatch(/registryName required/);
  });

  test('namespace must be a DNS-1123 label', () => {
    const rec = minimalRecord();
    rec.namespace = 'Not_A_Label';
    expect(validateRecord(rec).join('\n')).toMatch(/namespace "Not_A_Label" is not a DNS-1123 label/);
  });

  test('repos carry the owner/name reference shape', () => {
    const rec = minimalRecord();
    rec.repos = ['owner/repo', 'no-slash'];
    expect(validateRecord(rec).join('\n')).toMatch(/repo "no-slash" is not an "owner\/name" reference/);
  });

  test('cutover admits done or absent only', () => {
    const rec = minimalRecord();
    rec.cutover = 'pending';
    expect(validateRecord(rec).join('\n')).toMatch(/cutover must be "done" or absent/);
  });

  test('imageConflictPolicy is a closed vocabulary at record level', () => {
    const rec = minimalRecord();
    rec.imageConflictPolicy = 'prefer-everything';
    expect(validateRecord(rec).join('\n')).toMatch(/imageConflictPolicy must be "fail-closed"/);
  });
});

describe('workflow shape (validateWorkflowShape)', () => {
  test('type=image is image-only: no url, no ref, no interface', () => {
    const errs = validateWorkflowShape('ship', {
      type: 'image',
      image: 'img:tag',
      url: 'https://example/wf',
      ref: 'main',
      interface: emptyInterface(),
    });
    const all = errs.join('\n');
    expect(all).toMatch(/type=image must not declare url or ref/);
    expect(all).toMatch(/type=image must not declare interface/);
  });

  test('type=image requires an image', () => {
    expect(validateWorkflowShape('ship', { type: 'image' }).join('\n')).toMatch(
      /type=image requires an image/,
    );
  });

  test('type=url requires url and interface (KB-310)', () => {
    const errs = validateWorkflowShape('ship', { type: 'url' }).join('\n');
    expect(errs).toMatch(/type=url requires a url/);
    expect(errs).toMatch(/type=url requires interface/);
  });

  test('workflow name and class vocabularies mirror the server', () => {
    const errs = validateWorkflowShape('Not_Label', { type: 'image', image: 'i', class: 'triage' }).join('\n');
    expect(errs).toMatch(/is not a DNS-1123 label/);
    expect(validateWorkflowShape('ship', { type: 'image', image: 'i', class: 'chaos' }).join('\n')).toMatch(
      /class must be "dev" or "triage"/,
    );
  });

  test('volume and secret names are unique within the workflow', () => {
    const wf: Workflow = {
      type: 'image',
      image: 'i',
      volumes: [
        { name: 'work', kind: 'tmp', mountPath: '/tmp/work' },
        { name: 'work', kind: 'tmp', mountPath: '/tmp/other' },
      ],
      secrets: [
        { name: 'creds', secretProviderClass: 'spc', mountPath: '/secrets' },
        { name: 'creds', secretProviderClass: 'spc', mountPath: '/alt' },
      ],
    };
    const errs = validateWorkflowShape('ship', wf).join('\n');
    expect(errs).toMatch(/volumes\[1\] \(work\): name is not unique within the workflow/);
    expect(errs).toMatch(/secrets\[1\] \(creds\): name is not unique within the workflow/);
  });

  test('adapterImages reject blank or whitespace references', () => {
    const errs = validateWorkflowShape('ship', {
      type: 'image',
      image: 'i',
      adapterImages: { reviewer: ' ' },
    }).join('\n');
    expect(errs).toMatch(/adapterImages\.reviewer must be a non-empty image reference without whitespace/);
  });
});

describe('interface shape (interfaceErrors, KB-310)', () => {
  test('a declared interface validates clean', () => {
    const errs = interfaceErrors({
      apiVersion: API_VERSION,
      kind: 'WorkflowInterface',
      workflow: 'ship-it',
      adapters: ['reviewer'],
      mounts: [{ name: 'work', mountPath: '/work', kind: 'tmp' }],
      secretEnv: ['SHIPS_KEY'],
      env: ['BUILD_TOOL'],
      configSlots: [{ slot: 'buildCmd', variable: 'build_cmd', target: 'steps.build' }],
    });
    expect(errs).toEqual([]);
  });

  test('mount kinds are pvc|tmp and paths are absolute without trailing slash', () => {
    const errs = interfaceErrors({
      apiVersion: API_VERSION,
      kind: 'WorkflowInterface',
      workflow: 'ship',
      adapters: [],
      mounts: [
        { name: 'work', mountPath: 'work/rel', kind: 'nfs' },
        { name: 'slash', mountPath: '/fs/', kind: 'pvc' },
      ],
      secretEnv: [],
      env: [],
      configSlots: [],
    }).join('\n');
    expect(errs).toMatch(/must be an absolute path without trailing slash/);
    expect(errs).toMatch(/kind "nfs" must be pvc or tmp/);
  });

  test('interface env lanes are UPPER_SNAKE and config slots map the KB-310 surface', () => {
    const errs = interfaceErrors({
      apiVersion: API_VERSION,
      kind: 'WorkflowInterface',
      workflow: 'ship',
      adapters: [],
      mounts: [],
      secretEnv: ['lower-case'],
      env: ['bad-lower'],
      configSlots: [
        { slot: 'buildCmd', variable: 'WRONG', target: 'steps' },
        { slot: 'buildCmd', variable: 'build_cmd', target: 'steps2' },
        { slot: 'nonsense', variable: 'nonsense', target: 'steps3' },
      ],
    }).join('\n');
    expect(errs).toMatch(/secretEnv "lower-case" is not an env name \(UPPER_SNAKE\)/);
    expect(errs).toMatch(/env "bad-lower" is not an env name \(UPPER_SNAKE\)/);
    expect(errs).toMatch(/variable "WRONG" is not the slot's delivery variable "build_cmd"/);
    expect(errs).toMatch(/slot is not a platform config key/);
    expect(errs).toMatch(/configSlots\[1\] \(buildCmd\): slot is not unique/);
  });

  test('adapters and mounts must not repeat names', () => {
    const errs = interfaceErrors({
      apiVersion: API_VERSION,
      kind: 'WorkflowInterface',
      workflow: 'ship',
      adapters: ['reviewer', 'reviewer'],
      mounts: [
        { name: 'work', mountPath: '/work', kind: 'tmp' },
        { name: 'work', mountPath: '/work2', kind: 'tmp' },
      ],
      secretEnv: [],
      env: [],
      configSlots: [],
    }).join('\n');
    expect(errs).toMatch(/adapter kinds are not unique/);
    expect(errs).toMatch(/mounts\[1\] \(work\): name is not unique/);
  });
});

function emptyInterface() {
  return {
    apiVersion: API_VERSION,
    kind: 'WorkflowInterface',
    workflow: 'ship-it',
    adapters: [],
    mounts: [],
    secretEnv: [],
    env: [],
    configSlots: [],
  };
}

describe('volume + config entry shapes', () => {
  test('volume kinds are fail-closed against sibling fields', () => {
    expect(validateVolume({ name: 'work', kind: 'pvc', mountPath: '/w', claim: 'c', server: 'nfs-host' }).join('\n')).toMatch(
      /kind=pvc must not declare server, path, sizeLimit, or secretName/,
    );
    expect(validateVolume({ name: 'work', kind: 'nfs', mountPath: '/w' }).join('\n')).toMatch(
      /kind=nfs requires server and path/,
    );
    expect(validateVolume({ name: 'work', kind: 'tmp', mountPath: '/w', claim: 'c' }).join('\n')).toMatch(
      /kind=tmp must not declare claim, server, path, or secretName/,
    );
    expect(validateVolume({ name: 'work', kind: 'k8s-secret', mountPath: '/w' }).join('\n')).toMatch(
      /kind=k8s-secret requires a secretName/,
    );
    expect(validateVolume({ name: 'work', kind: 'unknown', mountPath: '/w' }).join('\n')).toMatch(
      /kind must be "pvc", "nfs", "tmp", or "k8s-secret"/,
    );
  });

  test('config entries reject blank-but-present commands and whitespace branches', () => {
    const errs = validateConfigEntryShape('cfg', {
      buildCmd: '   ',
      baseBranch: 'main branch',
      tags: ['ok', ''],
      tagMatch: 'sometimes',
    }).join('\n');
    expect(errs).toMatch(/buildCmd must not be blank when declared/);
    expect(errs).toMatch(/baseBranch "main branch" must be a git ref name without whitespace/);
    expect(errs).toMatch(/tags must not contain empty names/);
    expect(errs).toMatch(/tagMatch must be "all" or "any"/);
  });

  test('absent config fields are fine; declared ones are not blank-blocked', () => {
    expect(validateConfigEntryShape('cfg', { buildCmd: 'make build' })).toEqual([]);
  });
});

describe('binding shapes (bindingErrors)', () => {
  test('apiVersion/kind are fixed', () => {
    const b: Binding = { apiVersion: 'x', kind: 'y', images: {}, env: {}, volumes: [], secrets: [], config: {} };
    expect(bindingErrors(b).join('\n')).toMatch(/binding apiVersion must be "criteria\.brokenbots\.dev\/v1"/);
    expect(bindingErrors(b).join('\n')).toMatch(/binding kind must be "CriteriaBinding"/);
  });

  test('config slots are the KB-310 surface and values non-empty', () => {
    const b: Binding = {
      apiVersion: API_VERSION,
      kind: 'CriteriaBinding',
      images: {},
      env: {},
      volumes: [],
      secrets: [],
      config: { bogus: '', buildCmd: 'make' },
    };
    const all = bindingErrors(b).join('\n');
    expect(all).toMatch(/binding config slot "bogus" is not one of the platform config keys/);
    expect(all).toMatch(/binding config\.bogus must be a string value/);
  });

  test('secrets carry secretProviderClass and key-name env lanes', () => {
    const b: Binding = {
      apiVersion: API_VERSION,
      kind: 'CriteriaBinding',
      images: {},
      env: { 'bad!name': 'x' },
      volumes: [],
      secrets: [
        { name: 'creds', secretProviderClass: 'spc', mountPath: '/secrets', env: { CREDS_FILE: 'bad key!' } },
      ],
      config: {},
    };
    const all = bindingErrors(b).join('\n');
    expect(all).toMatch(/binding env has invalid env var name "bad!name"/);
    expect(all).toMatch(/env\.CREDS_FILE value "bad key!" is not a valid key name/);
  });

  test('a volume and a secret may share a name; duplicates within one list reject', () => {
    const b: Binding = {
      apiVersion: API_VERSION,
      kind: 'CriteriaBinding',
      images: {},
      env: {},
      volumes: [
        { name: 'work', kind: 'tmp', mountPath: '/v' },
        { name: 'work', kind: 'tmp', mountPath: '/v2' },
      ],
      secrets: [{ name: 'work', secretProviderClass: 'spc', mountPath: '/s', env: {} }],
      config: {},
    };
    const all = bindingErrors(b).join('\n');
    expect(all).toMatch(/binding volumes\[1\] duplicates name "work"/);
    expect(all).not.toMatch(/binding secrets\[0\] duplicates/);
  });
});

describe('record-level cross references', () => {
  test('route workflow must resolve in the record catalog', () => {
    const rec = minimalRecord();
    rec.routes.push({ name: 'ghost', workflow: 'nope', project: 'squad', states: [DEFAULT_STATE] });
    expect(validateRecord(rec).join('\n')).toMatch(
      /routes\[1\] \(ghost\): workflow "nope" is not present in the record's catalog/,
    );
  });

  test('route names are DNS-1123 labels, unique, and carry the record project', () => {
    const rec = minimalRecord();
    rec.routes.push({ ...newRoute('squad'), name: 'Bad_Name' });
    rec.routes.push({ ...newRoute('squad'), name: 'triage-runs' });
    rec.routes.push({ ...newRoute('other') });
    const all = validateRecord(rec).join('\n');
    expect(all).toMatch(/routes\[1\]\.name "Bad_Name" is not a DNS-1123 label/);
    expect(all).toMatch(/routes\[2\]\.name "triage-runs" is not unique/);
    expect(all).toMatch(/routes\[3\][^:]*: project "other" must be the record's registryName "squad"/);
  });

  test('states are not materialized: an empty or omitted list rejects', () => {
    const rec = minimalRecord();
    rec.routes.push({ ...newRoute('squad'), name: 'no-states', states: [] });
    expect(validateRecord(rec).join('\n')).toMatch(
      /routes\[1\] \(no-states\): states must not be empty \(records store states materialized: the payload default is \[Triage\]\)/,
    );
  });

  test('route configRef resolves and interface-declared slots gate entry fields', () => {
    const rec = minimalRecord();
    rec.catalog['ship-it'] = {
      type: 'url',
      url: 'https://git.example/squad/ship-it',
      interface: { ...emptyInterface(), workflow: 'ship-it', configSlots: [] },
    };
    rec.configLibrary = { 'cfg-a': { buildCmd: 'make build' } };
    rec.routes[0].configRef = 'cfg-missing';
    rec.routes.push({ ...newRoute('squad'), name: 'second', workflow: 'ship-it', configRef: 'cfg-a' });
    const all = validateRecord(rec).join('\n');
    expect(all).toMatch(/routes\[0\] \(triage-runs\): configRef "cfg-missing" is not present in the record's configLibrary/);
    expect(all).toMatch(
      /configRef "cfg-a": route "second" workflow "ship-it" interface does not declare the config slot "buildCmd"/,
    );
  });

  test('digests need a catalog entry and a sha256 hex64 body', () => {
    const rec = minimalRecord();
    rec.digests = { ghost: 'a'.repeat(64), 'ship-it': 'nothex', 'not-in-catalog': 'z'.repeat(64) };
    const all = validateRecord(rec).join('\n');
    expect(all).toMatch(/digests\["ghost"\]: no catalog entry named "ghost"/);
    expect(all).toMatch(/digests\["ship-it"\]: "nothex" is not a sha256 hex digest/);
    expect(all).toMatch(/digests\["not-in-catalog"\]: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz" is not a sha256 hex digest/);
  });
});

describe('unknown-field rejection (ParseRecord discipline)', () => {
  test('reject unknown keys at every level', () => {
    const rec = minimalRecord() as unknown as { [k: string]: unknown };
    rec.extraTop = 1;
    (rec.catalog as { [k: string]: { [k: string]: unknown } })['ship-it'].bogus = true;
    (rec.routes as unknown[]).push({ name: 'x', workflow: 'x', project: 'x', states: ['a'], sneaky: 1 });
    const errs = unknownFieldErrors(rec).join('\n');
    expect(errs).toMatch(/record: unknown field "extraTop"/);
    expect(errs).toMatch(/catalog\["ship-it"\]: unknown field "bogus"/);
    expect(errs).toMatch(/routes\[1\]: unknown field "sneaky"/);
  });

  test('the clean record and its nested shapes carry no unknown fields', () => {
    const rec = minimalRecord();
    rec.binding = { apiVersion: API_VERSION, kind: 'CriteriaBinding', images: {}, env: {}, volumes: [], secrets: [], config: {} };
    rec.catalog['ship-it'].interface = { ...emptyInterface(), workflow: 'ship-it' };
    rec.catalog['ship-it'].volumes = [{ name: 'work', kind: 'tmp', mountPath: '/w' }];
    expect(unknownFieldErrors(rec)).toEqual([]);
  });

  test('the live preview path flags a catalog interface carrying unknown keys', () => {
    const rec = minimalRecord();
    const wf: Workflow = { ...rec.catalog['ship-it'], type: 'url', url: 'https://git.example/squad/ship-it' };
    wf.interface = {
      ...emptyInterface(),
      workflow: 'ship-it',
      secrets: { KUBECONFIG: 'creds/kubeconfig' },
    } as Interface;
    rec.catalog['ship-it'] = wf;
    // Pastes with `secrets` instead of the schema's `secretEnv` are valid
    // JSON: ParseRecord rejects them fail-closed before Validate runs, so
    // the preview must lead with the unknown-field error.
    const errs = validateRecord(rec);
    expect(errs[0]).toMatch(/catalog\["ship-it"\]\.interface: unknown field "secrets"/);
    expect(errs.join('\n')).not.toMatch(/must not declare an interface/);
  });
});