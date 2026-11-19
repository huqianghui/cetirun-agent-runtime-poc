// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Title1,
  Button,
  Spinner,
  MessageBar,
  Card,
  CardHeader,
  Text,
  Badge,
  Tooltip,
} from "@fluentui/react-components";
import { Add20Regular } from "@fluentui/react-icons";
import { useInstancesQuery } from "../hooks/useInstance";
import { useFeatures } from "../hooks/useFeatures";
import { CreateForm } from "../components/CreateForm";
import { GovernanceNotice } from "../components/GovernanceNotice";
import { agentTypeLabel } from "../utils/instance";

export function InstanceListPage() {
  const navigate = useNavigate();
  const { data: instances, isLoading, error } = useInstancesQuery();
  const { data: features } = useFeatures();
  const [showCreate, setShowCreate] = useState(false);

  const maxInstances = features?.maxInstancesPerUser ?? 1;
  const atLimit = maxInstances > 0 && (instances?.length ?? 0) >= maxInstances;

  return (
    <div style={{ flex: 1, minHeight: 0, overflowY: "auto", padding: 24 }}>
      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          marginBottom: 24,
        }}
      >
        <Title1>My Agents</Title1>
        <Tooltip
          content={`Limit of ${maxInstances} agent${maxInstances > 1 ? "s" : ""} reached`}
          relationship="description"
          visible={atLimit ? undefined : false}
        >
          <Button
            icon={<Add20Regular />}
            appearance="primary"
            disabled={atLimit}
            onClick={() => setShowCreate(true)}
          >
            New Agent
          </Button>
        </Tooltip>
      </div>

      {isLoading && <Spinner label="Loading agents..." />}

      {error && (
        <MessageBar intent="error">
          Failed to load agents: {(error as Error).message}
        </MessageBar>
      )}

      {showCreate && (
        <div style={{ marginBottom: 24 }}>
          <CreateForm onCreated={() => setShowCreate(false)} />
          <Button
            style={{ marginTop: 8 }}
            onClick={() => setShowCreate(false)}
          >
            Cancel
          </Button>
        </div>
      )}

      {instances && instances.length === 0 && !showCreate && (
        <MessageBar intent="info">
          You don&apos;t have any agents yet. Create one to get started.
        </MessageBar>
      )}

      <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
        {instances?.map((inst) => (
          <Card
            key={inst.instanceName}
            onClick={() => navigate(`/instances/${inst.instanceName}/overview`)}
            style={{ cursor: "pointer", padding: "20px 24px" }}
          >
            <CardHeader
              header={<Text size={500} weight="semibold">{inst.instanceName}</Text>}
              description={<Text size={300}>{inst.displayName}</Text>}
              action={
                <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
                  <Badge appearance="outline">{agentTypeLabel(inst.agent.type)}</Badge>
                  <Badge
                    appearance="filled"
                    color={inst.phase === "Ready" ? "success" : "warning"}
                  >
                    {inst.phase}
                  </Badge>
                </div>
              }
            />
            <GovernanceNotice instance={inst} compact />
          </Card>
        ))}
      </div>
    </div>
  );
}
