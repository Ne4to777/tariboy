import { apiOn, apiRawOn, resolveTarget, type ApiTarget } from "./api";
import type { WorkflowBuildResult } from "./tasks";

export interface TeamRow { name: string; lead: string; members: number }
export interface TeamDetail { name: string; lead: string; members: string[]; broadcast: string; inbox: string; shared_dir: string }
export interface TeamImportResult { groups: number; agents: Array<{name: string; status: "created"|"reused"|"failed"; error?: string}>; complete: boolean; import_id?: string }
export interface TeamImportOperation { import_id: string; team: string; status: "pending"|"running"|"complete"|"failed"; steps: Array<{kind: "image"|"agent"; name: string; status: string; error?: string}>; error?: string; updated_at: string }
export interface TeamImportPreview { import_id: string; team: string; yaml: string; images: Array<{ref: string; action: string; conflict: boolean; message?: string}>; agents: Array<{name: string; action: string; conflict: boolean; message?: string}>; groups: Array<{name: string; action: string; conflict: boolean; message?: string}> }

const explicit = (target: ApiTarget) => resolveTarget(target);
export const listTeamsOn = (target: ApiTarget) => apiOn<{groups: TeamRow[]; count: number}>(explicit(target), "GET", "/api/groups");
export const getTeamOn = (target: ApiTarget, name: string) => apiOn<TeamDetail>(explicit(target), "GET", `/api/groups/${encodeURIComponent(name)}`);
export const renameTeamOn = (target: ApiTarget, name: string, newName: string) => apiOn<TeamDetail>(explicit(target), "PATCH", `/api/groups/${encodeURIComponent(name)}/name`, {new_name: newName});
export const setTeamLeadOn = (target: ApiTarget, name: string, lead: string) => apiOn<TeamDetail>(explicit(target), "PATCH", `/api/groups/${encodeURIComponent(name)}/lead`, {lead});
export const addExistingMemberOn = (target: ApiTarget, name: string, agent: string) => apiOn(explicit(target), "POST", `/api/groups/${encodeURIComponent(name)}/assign`, {agent});
export const removeTeamMemberOn = (target: ApiTarget, name: string, agent: string) => apiOn(explicit(target), "DELETE", `/api/groups/${encodeURIComponent(name)}/members/${encodeURIComponent(agent)}`);
export const getTeamComposeOn = (target: ApiTarget, name: string) => apiOn<{name: string; yaml: string}>(explicit(target), "GET", `/api/groups/${encodeURIComponent(name)}/compose`);
export const importTeamYAMLOn = (target: ApiTarget, yaml: string) => apiOn<TeamImportPreview>(explicit(target), "POST", "/api/team-imports/preview-yaml", {yaml});
export const applyTeamArchiveOn = (target: ApiTarget, importID: string, resolution?: {refs?: Record<string, string>; yaml?: string; update_existing?: boolean}) => apiOn<TeamImportResult>(explicit(target), "POST", `/api/team-imports/${encodeURIComponent(importID)}/apply`, resolution ?? {});
export const getTeamImportOperationOn = (target: ApiTarget, importID: string) => apiOn<TeamImportOperation>(explicit(target), "GET", `/api/team-imports/${encodeURIComponent(importID)}`);
export const downloadTeamArchiveOn = async (target: ApiTarget, name: string) => (await apiRawOn(explicit(target), "GET", `/api/groups/${encodeURIComponent(name)}/export`)).blob();
export const uploadTeamArchiveOn = async (target: ApiTarget, archive: Blob) => {
  const response = await apiRawOn(explicit(target), "POST", "/api/team-imports", archive);
  const envelope = await response.json() as {result: TeamImportPreview};
  return {...envelope.result, agents: envelope.result.agents ?? [], groups: envelope.result.groups ?? []};
};
export const downloadImageArchiveOn = async (target: ApiTarget, ref: string) => (await apiRawOn(explicit(target), "GET", `/api/images/${encodeURIComponent(ref)}/export`)).blob();
export const uploadImageArchiveOn = async (target: ApiTarget, archive: Blob) => {
  const response = await apiRawOn(explicit(target), "POST", "/api/image-imports", archive);
  return ((await response.json()) as {result: {import_id: string; ref: string; digest: string}}).result;
};
export const applyImageArchiveOn = (target: ApiTarget, importID: string, ref?: string) => apiOn(explicit(target), "POST", `/api/image-imports/${encodeURIComponent(importID)}/apply`, ref ? {ref} : {});
export const downloadWorkflowArchiveOn = async (target: ApiTarget, name: string, tag: string) =>
  (await apiRawOn(explicit(target), "GET", `/api/workflow-images/${encodeURIComponent(name)}/${encodeURIComponent(tag)}/export`)).blob();
export const importWorkflowArchiveOn = async (target: ApiTarget, archive: Blob) => {
  const response = await apiRawOn(explicit(target), "POST", "/api/workflow-image-imports", archive);
  return ((await response.json()) as {result: WorkflowBuildResult}).result;
};
