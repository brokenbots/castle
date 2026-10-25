import { createApi } from '@reduxjs/toolkit/query/react';
import { fakeBaseQuery } from '@reduxjs/toolkit/query';
import { ConnectError } from '@connectrpc/connect';
import { connectCodeName, projectsClient, tsToIso } from '@castle/run-viewer';
import type {
  Project as PbProject,
  ProjectMember as PbProjectMember,
  RefirePolicy as PbRefirePolicy,
} from '@castle/run-viewer/src/gen/castle/v1/projects_pb';
import {
  ProjectMemberKind,
  ProjectMembershipPolicy,
} from '@castle/run-viewer/src/gen/castle/v1/projects_pb';

// The settings console rides castle.v1.ProjectService (KB-324): projects,
// settings (etag swap), refire policies and membership. The console session
// the operator logged in with carries these writes; protocol tokens are
// denied server-side, so this slice maps wire errors for the UI instead of
// re-implementing authorization.

export const SETTINGS_API_REDUCER_PATH = 'settingsApi';

export type MembershipPolicy = 'open' | 'closed';
export type MemberKind = 'ai' | 'human';

export interface ProjectInfo {
  id: string;
  membershipPolicy: MembershipPolicy;
  defaultRefirePolicy: string;
  queues: string[];
  settings: Record<string, string>;
  info: Record<string, string>;
  createdAt?: string;
  updatedAt?: string;
}

export interface ProjectMemberInfo {
  projectId: string;
  memberId: string;
  kind: MemberKind;
  createdAt?: string;
}

export interface ProjectSettingsDoc {
  settings: Record<string, string>;
  /** Content-addressed etag; '' when the doc is unset. */
  etag: string;
}

export interface SetProjectSettingsArgs {
  projectId: string;
  settings: Record<string, string>;
  expectedEtag: string;
}

export interface RefirePolicyDoc {
  maxAttempts: number;
  backoffSeconds: number;
  transient: string[];
  parks: string[];
  cosmetics: string[];
}

export interface RefirePolicyEntry {
  policy: RefirePolicyDoc;
  updatedAt?: string;
}

export interface RefirePolicyArgs {
  projectId: string;
  /** '' is the project-scoped default row. */
  route: string;
}

export interface SetRefirePolicyArgs extends RefirePolicyArgs {
  policy: RefirePolicyDoc;
}

export interface AddMemberArgs {
  projectId: string;
  memberId: string;
  kind: MemberKind;
}

export interface RemoveMemberArgs {
  projectId: string;
  memberId: string;
}

function toError(err: unknown) {
  if (err instanceof ConnectError) {
    return { status: connectCodeName(err.code), data: err.rawMessage };
  }
  return { status: 'CUSTOM_ERROR', data: err instanceof Error ? err.message : String(err) };
}

function mapPolicy(p: ProjectMembershipPolicy): MembershipPolicy {
  return p === ProjectMembershipPolicy.OPEN ? 'open' : 'closed';
}

function mapKind(k: ProjectMemberKind): MemberKind {
  return k === ProjectMemberKind.HUMAN ? 'human' : 'ai';
}

function mapProject(p: PbProject): ProjectInfo {
  return {
    id: p.id,
    membershipPolicy: mapPolicy(p.membershipPolicy),
    defaultRefirePolicy: p.defaultRefirePolicy,
    queues: [...p.queues],
    settings: { ...p.settings },
    info: { ...p.info },
    createdAt: tsToIso(p.createdAt),
    updatedAt: tsToIso(p.updatedAt),
  };
}

function mapMember(m: PbProjectMember): ProjectMemberInfo {
  return {
    projectId: m.projectId,
    memberId: m.memberId,
    kind: mapKind(m.kind),
    createdAt: tsToIso(m.createdAt),
  };
}

function mapRefirePolicy(p: PbRefirePolicy): RefirePolicyDoc {
  return {
    maxAttempts: p.maxAttempts,
    backoffSeconds: p.backoffSeconds,
    transient: [...p.transient],
    parks: [...p.parks],
    cosmetics: [...p.cosmetics],
  };
}

/** Tag id for a per-(project, route) refire policy; '' route = default row. */
export function policyId(projectId: string, route: string): string {
  return `${projectId}/${route}`;
}

export const settingsApi = createApi({
  reducerPath: SETTINGS_API_REDUCER_PATH,
  baseQuery: fakeBaseQuery<{ status: string | number; data: string }>(),
  tagTypes: ['Project', 'ProjectSettings', 'RefirePolicy', 'ProjectMember'],
  endpoints: (b) => ({
    listProjects: b.query<ProjectInfo[], void>({
      queryFn: async () => {
        try {
          const resp = await projectsClient.listProjects({});
          return { data: resp.projects.map(mapProject) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: [{ type: 'Project', id: 'LIST' }],
    }),
    getProject: b.query<ProjectInfo, string>({
      queryFn: async (projectId) => {
        try {
          return { data: mapProject((await projectsClient.getProject({ projectId })).project as PbProject) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, projectId) => [{ type: 'Project', id: projectId }],
    }),
    createProject: b.mutation<ProjectInfo, { slug: string; membershipPolicy: MembershipPolicy }>({
      queryFn: async ({ slug, membershipPolicy }) => {
        try {
          const resp = await projectsClient.createProject({
            project: {
              id: slug,
              membershipPolicy:
                membershipPolicy === 'open'
                  ? ProjectMembershipPolicy.OPEN
                  : ProjectMembershipPolicy.CLOSED,
            },
          });
          return { data: mapProject(resp.project as PbProject) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: [{ type: 'Project', id: 'LIST' }],
    }),
    getProjectSettings: b.query<ProjectSettingsDoc, string>({
      queryFn: async (projectId) => {
        try {
          const resp = await projectsClient.getProjectSettings({ projectId });
          return { data: { settings: { ...resp.settings }, etag: resp.etag } };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, projectId) => [{ type: 'ProjectSettings', id: projectId }],
    }),
    setProjectSettings: b.mutation<string, SetProjectSettingsArgs>({
      queryFn: async ({ projectId, settings, expectedEtag }) => {
        try {
          const resp = await projectsClient.setProjectSettings({ projectId, settings, expectedEtag });
          return { data: resp.etag };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { projectId }) => [
        { type: 'ProjectSettings', id: projectId },
        { type: 'Project', id: projectId },
      ],
    }),
    getRefirePolicy: b.query<RefirePolicyEntry, RefirePolicyArgs>({
      queryFn: async ({ projectId, route }) => {
        try {
          const resp = await projectsClient.getRefirePolicy({ projectId, route });
          return { data: { policy: mapRefirePolicy(resp.policy as PbRefirePolicy), updatedAt: tsToIso(resp.updatedAt) } };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, { projectId, route }) => [{ type: 'RefirePolicy', id: policyId(projectId, route) }],
    }),
    setRefirePolicy: b.mutation<RefirePolicyEntry, SetRefirePolicyArgs>({
      queryFn: async ({ projectId, route, policy }) => {
        try {
          const resp = await projectsClient.setRefirePolicy({
            projectId,
            route,
            policy: {
              maxAttempts: policy.maxAttempts,
              backoffSeconds: policy.backoffSeconds,
              transient: policy.transient,
              parks: policy.parks,
              cosmetics: policy.cosmetics,
            },
          });
          return { data: { policy, updatedAt: tsToIso(resp.updatedAt) } };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { projectId, route }) => [{ type: 'RefirePolicy', id: policyId(projectId, route) }],
    }),
    listProjectMembers: b.query<ProjectMemberInfo[], string>({
      queryFn: async (projectId) => {
        try {
          const resp = await projectsClient.listProjectMembers({ projectId });
          return { data: resp.members.map(mapMember) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      providesTags: (_r, _e, projectId) => [{ type: 'ProjectMember', id: projectId }],
    }),
    addProjectMember: b.mutation<ProjectMemberInfo, AddMemberArgs>({
      queryFn: async ({ projectId, memberId, kind }) => {
        try {
          const resp = await projectsClient.addProjectMember({
            projectId,
            memberId,
            kind: kind === 'human' ? ProjectMemberKind.HUMAN : ProjectMemberKind.AI,
          });
          return { data: mapMember(resp.member as PbProjectMember) };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { projectId }) => [{ type: 'ProjectMember', id: projectId }],
    }),
    removeProjectMember: b.mutation<void, RemoveMemberArgs>({
      queryFn: async ({ projectId, memberId }) => {
        try {
          await projectsClient.removeProjectMember({ projectId, memberId });
          return { data: undefined };
        } catch (err) {
          return { error: toError(err) };
        }
      },
      invalidatesTags: (_r, _e, { projectId }) => [{ type: 'ProjectMember', id: projectId }],
    }),
  }),
});

export const {
  useListProjectsQuery,
  useGetProjectQuery,
  useCreateProjectMutation,
  useGetProjectSettingsQuery,
  useSetProjectSettingsMutation,
  useGetRefirePolicyQuery,
  useSetRefirePolicyMutation,
  useListProjectMembersQuery,
  useAddProjectMemberMutation,
  useRemoveProjectMemberMutation,
} = settingsApi;