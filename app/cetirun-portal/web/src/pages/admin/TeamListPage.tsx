// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Fragment, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Title2,
  Button,
  Spinner,
  MessageBar,
  Table,
  TableHeader,
  TableHeaderCell,
  TableRow,
  TableBody,
  TableCell,
  Dialog,
  DialogTrigger,
  DialogSurface,
  DialogTitle,
  DialogBody,
  DialogActions,
  DialogContent,
  Text,
} from "@fluentui/react-components";
import { Add20Regular, Delete20Regular, Edit20Regular } from "@fluentui/react-icons";
import { listTeams, deleteTeam, type TeamDTO } from "../../api/adminTeam";
import { CompatibilityBadge, CompatibilityReport } from "../../components/CompatibilityReport";

export function TeamListPage() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { data: teams, isLoading, error } = useQuery<TeamDTO[], Error>({
    queryKey: ["admin", "teams"],
    queryFn: listTeams,
    refetchInterval: 15_000,
  });
  const [pendingDelete, setPendingDelete] = useState<TeamDTO | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);

  const delMutation = useMutation({
    mutationFn: deleteTeam,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "teams"] });
      setPendingDelete(null);
    },
    onError: (e: Error) => setDeleteError(e.message),
  });

  return (
    <div>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          marginBottom: 16,
        }}
      >
        <Title2>Teams</Title2>
        <Button
          appearance="primary"
          icon={<Add20Regular />}
          onClick={() => navigate("/admin/teams/new")}
        >
          New Team
        </Button>
      </div>
      <Text block style={{ marginBottom: 16 }}>
        Create one Team for each group of people. Teams that follow the same rules share an Agent Policy and Sandboxes.
        A Team keeps no member list: an Agent's owner chooses its Team. In the AI Gateway console, a Team's usage and
        any budget set there appear under the team cetirun-team-&lt;name&gt;.
      </Text>

      {isLoading && <Spinner label="Loading teams..." />}
      {error && <MessageBar intent="error">Failed to load: {error.message}</MessageBar>}
      {deleteError && <MessageBar intent="error">{deleteError}</MessageBar>}

      {teams && (
        <Table aria-label="Teams">
          <TableHeader>
            <TableRow>
              <TableHeaderCell>Name</TableHeaderCell>
              <TableHeaderCell>Role</TableHeaderCell>
              <TableHeaderCell>Agent Policy</TableHeaderCell>
              <TableHeaderCell>Default Sandbox</TableHeaderCell>
              <TableHeaderCell>Compatibility</TableHeaderCell>
              <TableHeaderCell>Skills</TableHeaderCell>
              <TableHeaderCell style={{ width: 120 }}>Actions</TableHeaderCell>
            </TableRow>
          </TableHeader>
          <TableBody>
            {teams.length === 0 && (
              <TableRow>
                  <TableCell colSpan={7}>
                  <Text>No teams yet.</Text>
                </TableCell>
              </TableRow>
            )}
            {teams.map((t) => <Fragment key={t.name}>
              <TableRow>
                <TableCell>{t.name}</TableCell>
                <TableCell>{t.role || "—"}</TableCell>
                <TableCell>{t.agentPolicyRef || "—"}</TableCell>
                <TableCell>{t.defaultSandboxRef || "—"}</TableCell>
                <TableCell><CompatibilityBadge team={t} /></TableCell>
                <TableCell>{t.skills?.length ?? 0}</TableCell>
                <TableCell>
                  <Button size="small" appearance="subtle" onClick={() => setExpanded(expanded === t.name ? null : t.name)} aria-expanded={expanded === t.name} aria-label={`Show compatibility for ${t.name}`}>Details</Button>
                  <Button
                    size="small"
                    icon={<Edit20Regular />}
                    appearance="subtle"
                    disabled={t.deleting}
                    onClick={() => navigate(`/admin/teams/${encodeURIComponent(t.name)}/edit`)}
                    aria-label={`Edit ${t.name}`}
                  />
                  <Button
                    size="small"
                    icon={<Delete20Regular />}
                    appearance="subtle"
                    disabled={t.deleting}
                    onClick={() => {
                      setDeleteError(null);
                      setPendingDelete(t);
                    }}
                    aria-label={`Delete ${t.name}`}
                  />
                </TableCell>
              </TableRow>
              {expanded === t.name && <TableRow><TableCell colSpan={7}><CompatibilityReport team={t} /></TableCell></TableRow>}
            </Fragment>)}
          </TableBody>
        </Table>
      )}

      <Dialog
        open={pendingDelete !== null}
        onOpenChange={(_, data) => {
          if (!data.open) setPendingDelete(null);
        }}
      >
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Delete team?</DialogTitle>
            <DialogContent>
              Are you sure you want to delete team{" "}
              <strong>{pendingDelete?.name}</strong>? This cannot be undone.
            </DialogContent>
            <DialogActions>
              <DialogTrigger disableButtonEnhancement>
                <Button appearance="secondary">Cancel</Button>
              </DialogTrigger>
              <Button
                appearance="primary"
                disabled={delMutation.isPending}
                onClick={() => { if (pendingDelete) delMutation.mutate(pendingDelete); }}
              >
                Delete
              </Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
    </div>
  );
}
