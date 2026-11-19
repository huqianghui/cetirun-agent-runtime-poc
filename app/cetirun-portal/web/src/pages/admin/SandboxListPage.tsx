// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Fragment, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Button,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
  DialogTrigger,
  MessageBar,
  Spinner,
  Table,
  TableBody,
  TableCell,
  TableHeader,
  TableHeaderCell,
  TableRow,
  Text,
  Title2,
} from "@fluentui/react-components";
import { Add20Regular, Delete20Regular, Edit20Regular } from "@fluentui/react-icons";
import { deleteSandbox, listSandboxes, type SandboxDTO } from "../../api/adminSandbox";
import { listTeams, type TeamDTO } from "../../api/adminTeam";
import { CompatibilityReferences } from "../../components/CompatibilityReport";

function readyCondition(box: SandboxDTO) {
  return box.conditions?.find((condition) => condition.type === "Ready" && condition.observedGeneration === box.generation);
}

function readinessDetails(box: SandboxDTO) {
  const condition = readyCondition(box);
  if (!condition || condition.status === "True" || (!condition.reason && !condition.message)) return undefined;
  return [condition.reason, condition.message].filter(Boolean).join(": ");
}

export function SandboxListPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { data: sandboxes, isLoading, error } = useQuery<SandboxDTO[], Error>({
    queryKey: ["admin", "sandboxes"],
    queryFn: listSandboxes,
    refetchInterval: 15_000,
  });
  const teamsQuery = useQuery<TeamDTO[], Error>({ queryKey: ["admin", "teams"], queryFn: listTeams, refetchInterval: 15_000 });
  const [pendingDelete, setPendingDelete] = useState<SandboxDTO | null>(null);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<string | null>(null);
  const deleteMutation = useMutation({
    mutationFn: (box: SandboxDTO) => deleteSandbox(box),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["admin", "sandboxes"] });
      setPendingDelete(null);
    },
    onError: (error: Error) => setDeleteError(error.message),
  });

  return (
    <div>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: 16 }}>
        <Title2>Sandboxes</Title2>
        <Button appearance="primary" icon={<Add20Regular />} onClick={() => navigate("/admin/sandboxes/new")}>
          New Sandbox
        </Button>
      </div>
      <Text block style={{ marginBottom: 16 }}>
        Sandboxes define reusable runtime, resources and storage. Assign them to a Team after creating its Agent Policy.
      </Text>
      {isLoading && <Spinner label="Loading Sandboxes..." />}
      {error && <MessageBar intent="error">Failed to load: {error.message}</MessageBar>}
      {deleteError && <MessageBar intent="error">{deleteError}</MessageBar>}
      {sandboxes && (
        <Table aria-label="Sandboxes">
          <TableHeader>
            <TableRow>
              <TableHeaderCell>Name</TableHeaderCell>
              <TableHeaderCell>Runtime</TableHeaderCell>
              <TableHeaderCell>HOME</TableHeaderCell>
              <TableHeaderCell>Ready</TableHeaderCell>
              <TableHeaderCell style={{ width: 120 }}>Actions</TableHeaderCell>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sandboxes.length === 0 && (
              <TableRow><TableCell colSpan={5}>No Sandboxes yet.</TableCell></TableRow>
            )}
            {sandboxes.map((box) => <Fragment key={box.name}>
              <TableRow>
                <TableCell>{box.name}</TableCell>
                <TableCell>{box.type}</TableCell>
                <TableCell>{box.storage.home.size} ({box.storage.home.storageClassName})</TableCell>
                <TableCell>{box.deleting ? "Deleting" : box.ready}</TableCell>
                <TableCell>
                  <Button size="small" appearance="subtle" onClick={() => setExpanded(expanded === box.name ? null : box.name)} aria-expanded={expanded === box.name} aria-label={`Show Teams using ${box.name}`}>Teams</Button>
                  <Button size="small" appearance="subtle" icon={<Edit20Regular />} aria-label={`Edit ${box.name}`} onClick={() => navigate(`/admin/sandboxes/${box.name}/edit`)} />
                  <Button size="small" appearance="subtle" icon={<Delete20Regular />} aria-label={`Delete ${box.name}`} onClick={() => { setDeleteError(null); setPendingDelete(box); }} />
                </TableCell>
              </TableRow>
              {expanded === box.name && <TableRow><TableCell colSpan={5}>
                {readinessDetails(box) && <Text block size={200} style={{ marginBottom: 8 }}>
                  Readiness: {readinessDetails(box)}
                </Text>}
                {teamsQuery.isLoading && <Spinner size="tiny" label="Loading referencing Teams..." />}
                {teamsQuery.error && <MessageBar intent="error">Failed to load Teams: {teamsQuery.error.message}</MessageBar>}
                {teamsQuery.data && <CompatibilityReferences teams={teamsQuery.data.filter((team) => team.sandboxRefs.includes(box.name))} sandboxName={box.name} dependency={{ kind: "sandbox", name: box.name, uid: box.uid, generation: box.generation, deleting: box.deleting }} />}
              </TableCell></TableRow>}
            </Fragment>)}
          </TableBody>
        </Table>
      )}
      <Dialog open={pendingDelete !== null} onOpenChange={(_, data) => { if (!data.open) setPendingDelete(null); }}>
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Delete Sandbox?</DialogTitle>
            <DialogContent>Remove <strong>{pendingDelete?.name}</strong>? Teams must stop referencing this Sandbox before it can be deleted.</DialogContent>
            <DialogActions>
              <DialogTrigger disableButtonEnhancement><Button appearance="secondary">Cancel</Button></DialogTrigger>
              <Button appearance="primary" disabled={deleteMutation.isPending || !pendingDelete} onClick={() => {
                if (pendingDelete) deleteMutation.mutate(pendingDelete);
              }}>Delete</Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
    </div>
  );
}
