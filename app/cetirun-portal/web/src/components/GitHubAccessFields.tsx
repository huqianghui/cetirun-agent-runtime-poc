// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Button, Checkbox, Field, Select, Spinner, Text } from "@fluentui/react-components";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { listGitHubRepositories, type Connection, type GitHubBinding, type GitHubRepositories, type GitHubRepository } from "../api/toolAuthorization";
import { permissionKeys, permissionLabel, permissionLabels } from "./serviceAccess";

export const validGitHubAccess = (value: GitHubBinding) => value.repositoryIds.length > 0 && value.repositoryIds.length <= 500 && permissionKeys.some((key) => value.permissions[key]);

/** Resolve selected names across pages without adding display data to the grant. */
export function useSelectedRepositories(connection: Connection | undefined, value: GitHubBinding) {
  const client = useQueryClient();
  const installationId = connection?.github?.installationId;
  const ids = [...value.repositoryIds].sort((a, b) => a - b);
  return useQuery({
    queryKey: ["github-selected-repositories", connection?.id, installationId, ids.join(",")],
    enabled: !!installationId && ids.length > 0,
    queryFn: async ({ signal }) => {
      const found = new Map<number, GitHubRepository>();
      // Every cached page belongs to this exact account and installation.
      for (const [, data] of client.getQueriesData<GitHubRepositories>({ queryKey: ["github-repositories", connection!.id, installationId] })) {
        for (const repo of data?.repositories ?? []) if (ids.includes(repo.id)) found.set(repo.id, repo);
      }
      let page = 1;
      while (ids.some((id) => !found.has(id))) {
        signal.throwIfAborted();
        const key = ["github-repositories", connection!.id, installationId, page];
        const data = client.getQueryData<GitHubRepositories>(key) ?? await listGitHubRepositories(connection!.id, installationId!, page, signal);
        client.setQueryData(key, data);
        for (const repo of data.repositories) if (ids.includes(repo.id)) found.set(repo.id, repo);
        if (!data.hasNextPage) break;
        page++;
      }
      return ids.map((id) => ({ id, repository: found.get(id) }));
    },
  });
}

export function GitHubAccessFields({ connection, value, onChange, disabled }: { connection: Connection; value: GitHubBinding; onChange: (value: GitHubBinding) => void; disabled: boolean }) {
  const [page, setPage] = useState(1);
  const installationId = connection.github!.installationId!;
  const repositories = useQuery({ queryKey: ["github-repositories", connection.id, installationId, page], queryFn: ({ signal }) => listGitHubRepositories(connection.id, installationId, page, signal) });
  const selected = useSelectedRepositories(connection, value);
  const remove = (id: number) => onChange({ ...value, repositoryIds: value.repositoryIds.filter((item) => item !== id) });
  return <>
    <Text as="h3" weight="semibold" block>Repositories</Text>
    <Text block size={200}>Choose the repositories this agent can access. No repositories are selected automatically.</Text>
    {repositories.isLoading && <Spinner size="tiny" label="Loading repositories..." />}
    {repositories.error && <div role="alert"><Text block>Could not load repositories.</Text><Button onClick={() => void repositories.refetch()}>Retry repositories</Button></div>}
    {repositories.data?.repositories.map((repo) => <Checkbox key={repo.id}
      label={repo.admin ? repo.fullName : `${repo.fullName} (repository admin required)`}
      checked={value.repositoryIds.includes(repo.id)}
      disabled={disabled || repositories.isError || (!value.repositoryIds.includes(repo.id) && (!repo.admin || value.repositoryIds.length >= 500))}
      onChange={(_, data) => data.checked === true ? onChange({ ...value, repositoryIds: [...new Set([...value.repositoryIds, repo.id])] }) : remove(repo.id)} />)}
    {repositories.data && !repositories.data.repositories.length && <Text block>No repositories are available. Check the App's repository selection in GitHub and your repository permissions.</Text>}
    <div style={{ display: "flex", flexWrap: "wrap", gap: 8, marginTop: 12 }}>
      <Button disabled={disabled || page === 1 || repositories.isFetching} onClick={() => setPage((current) => current - 1)}>Previous repositories</Button>
      <Button disabled={disabled || !repositories.data?.hasNextPage || repositories.isFetching || repositories.isError} onClick={() => setPage((current) => current + 1)}>More repositories</Button>
      <Text role="status">{value.repositoryIds.length} of 500 repositories selected</Text>
    </div>
    {!!value.repositoryIds.length && <details style={{ marginTop: 12 }}><summary>Selected repositories ({value.repositoryIds.length})</summary>
      {selected.isLoading && <Spinner size="tiny" label="Loading selected repositories..." />}
      {selected.error && <><Text role="alert" block>Could not load selected repositories.</Text><Button onClick={() => void selected.refetch()}>Retry selected repositories</Button></>}
      {selected.data?.map(({ id, repository }) => <div key={id} style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center" }}>
        <Text>{repository?.fullName ?? "Unavailable repository"}{repository && !repository.admin ? " (repository admin required)" : ""}</Text>
        <Button size="small" disabled={disabled} aria-label={`Remove ${repository?.fullName ?? `unavailable repository ${id}`}`} onClick={() => remove(id)}>Remove</Button>
      </div>)}
    </details>}
    <Text as="h3" weight="semibold" block style={{ marginTop: 24 }}>Permissions</Text>
    <Text size={200} block>These permissions apply to every selected repository.</Text>
    {permissionKeys.map((key) => <Field key={key} label={permissionLabels[key]} style={{ marginTop: 12 }}>
      <Select aria-label={`${permissionLabels[key]} permission`} value={value.permissions[key] ?? ""} disabled={disabled} onChange={(_, data) => {
        const permissions = { ...value.permissions };
        if (!data.value) delete permissions[key]; else permissions[key] = data.value as "read" | "write";
        onChange({ ...value, permissions });
      }}><option value="">No access</option><option value="read">Read</option><option value="write">Read and write</option></Select>
    </Field>)}
    <Text size={200} block style={{ marginTop: 12 }}>Select at least one permission. Basic repository metadata is included.</Text>
    {selected.isLoading && <Spinner size="tiny" label="Checking selected repositories..." />}
    {selected.isError && <Text role="alert" block>Could not check your selection. Expand Selected repositories to retry.</Text>}
    {selected.data?.some((item) => !item.repository?.admin) && <Text role="alert" block>Some selected repositories are unavailable or require administrator permission. Remove them from Selected repositories before continuing.</Text>}
  </>;
}

export function GitHubPermissionSummary({ value }: { value: GitHubBinding }) {
  return <Text size={200} block>{value.repositoryIds.length} {value.repositoryIds.length === 1 ? "repository" : "repositories"} · {permissionKeys.map((key) => `${permissionLabels[key]}: ${permissionLabel(value.permissions[key])}`).join(" · ")}</Text>;
}
