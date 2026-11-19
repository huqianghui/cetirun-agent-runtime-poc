// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useEffect, useState } from "react";
import {
  Button, Dialog, DialogActions, DialogBody, DialogContent, DialogSurface, DialogTitle, Text,
} from "@fluentui/react-components";
import { isSuspensionRequestRejected } from "../api/instance";
import { useSetInstanceSuspendedByName } from "../hooks/useInstance";
import type { Instance, SetInstanceSuspendedRequest } from "../types/instance";

export function SuspendAgentButton({ instance }: { instance: Instance }) {
  const change = useSetInstanceSuspendedByName(instance.instanceName);
  const [open, setOpen] = useState(false);
  const [request, setRequest] = useState<SetInstanceSuspendedRequest | null>(null);
  const sent = Boolean(request && change.variables);
  const observed = Boolean(sent && request && instance.uid === request.uid &&
    instance.suspended === request.suspended && instance.resourceVersion !== request.resourceVersion);
  const accepted = observed || Boolean(request && change.data?.uid === request.uid &&
    change.data.suspended === request.suspended);
  const rejected = change.isError && !accepted && isSuspensionRequestRejected(change.error);
  const available = instance.suspended ? instance.resumeAvailable : instance.suspendAvailable;
  const action = (request?.suspended ?? !instance.suspended) ? "Suspend" : "Resume";

  useEffect(() => {
    if (observed && !change.isPending) {
      setOpen(false);
      setRequest(null);
      change.reset();
    }
  }, [observed, change.isPending, change.reset]);

  const closeDialog = () => {
    if (change.isPending) return;
    setOpen(false);
    if (!sent || rejected) {
      setRequest(null);
      change.reset();
    }
  };

  return <>
    <Button disabled={change.isPending || (!request && !available)} onClick={() => {
      if (!request) {
        change.reset();
        setRequest({ uid: instance.uid, resourceVersion: instance.resourceVersion, generation: instance.generation,
          suspended: !instance.suspended });
      }
      setOpen(true);
    }}>
      {request && sent ? `Review ${action.toLowerCase()} request` : `${action} Agent`}
    </Button>
    {instance.suspended === false && !instance.suspendAvailable && instance.suspensionUnavailableReason &&
      <Text role="status" style={{ flexBasis: "100%", order: 1 }}>{instance.suspensionUnavailableReason}</Text>}
    {instance.suspended === true && <Text role="status" style={{ flexBasis: "100%", order: 1 }}>
      Suspension requested. This view does not confirm Pod termination or removal of every access path.
      The Agent and any existing HOME files or Gateway key are retained.
    </Text>}
    {instance.suspended === false && instance.suspendAvailable && !instance.restartAvailable &&
      <Text role={instance.phase === "Failed" ? "alert" : "status"} style={{ flexBasis: "100%", order: 1 }}>
        {instance.phase === "Failed" ? "The workload has not recovered." : "Waiting for the current workload to become ready."}
        {" "}Check Status for progress or failures. An accepted resume is not readiness.
      </Text>}
    <Dialog open={open} onOpenChange={(_, data) => {
      if (data.open) setOpen(true);
      else closeDialog();
    }}>
      <DialogSurface>
        <DialogBody>
          <DialogTitle>{action} Agent</DialogTitle>
          <DialogContent>
            {request?.suspended
              ? "Request suspension of this Agent? Active sessions will be interrupted. The Agent and any existing HOME files or Gateway key are retained."
              : "Request resumption with the current configuration? Existing restart or administrator migration requests continue through the controller; this does not create another operation."}
            <Text block>Acceptance changes the requested intent, not the observed workload state.</Text>
            {change.isError && <Text block role="alert">
              {change.error.message} {rejected
                ? "This attempt was rejected. Check the current Agent before confirming again; an earlier uncertain attempt may have taken effect."
                : "The outcome is uncertain. Retry sends the original action, UID and version. Closing this dialog does not cancel accepted work."}
            </Text>}
          </DialogContent>
          <DialogActions>
            <Button onClick={closeDialog} disabled={change.isPending}>{sent ? "Close" : "Cancel"}</Button>
            <Button appearance="primary" disabled={change.isPending || request === null || rejected || accepted}
              onClick={() => {
                if (request) change.mutate(request, { onSuccess: () => { setOpen(false); setRequest(null); } });
              }}>
              {change.isPending ? "Submitting request..." : rejected ? "Request rejected" : change.isError ? "Retry request" : action}
            </Button>
          </DialogActions>
        </DialogBody>
      </DialogSurface>
    </Dialog>
  </>;
}
