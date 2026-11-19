// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { lazy, Suspense } from "react";
import { useParams, useNavigate, Navigate } from "react-router-dom";
import {
  Button,
  Text,
  Badge,
  makeStyles,
  tokens,
} from "@fluentui/react-components";
import {
  ArrowLeft20Regular,
  Home20Regular,
  Chat20Regular,
  WindowConsole20Regular,
  Desktop20Regular,
} from "@fluentui/react-icons";
import { useInstanceByNameQuery } from "../hooks/useInstance";
import { useMe } from "../context/MeContext";
import { SESSION_EXPIRED_MESSAGE } from "../api/fetch";
import { OverviewSection } from "../components/OverviewSection";
import {
  agentUIUrl,
  desktopUrl,
  phaseBadgeColor,
  terminalUrl,
} from "../utils/instance";
import { useFeatures } from "../hooks/useFeatures";
import { useChatAccount } from "../chat/ChatAccount";

const ChatPage = lazy(() => import("../chat/ChatPage"));

type NavItem = {
  section: string;
  label: string;
  icon: React.ReactElement;
  external?: "agent" | "terminal" | "desktop";
};

const NAV_ITEMS: NavItem[] = [
  { section: "overview", label: "Overview", icon: <Home20Regular /> },
  { section: "chat", label: "Chat", icon: <Chat20Regular /> },
  {
    section: "agent-ui",
    label: "Agent UI",
    icon: <Chat20Regular />,
    external: "agent",
  },
  {
    section: "terminal",
    label: "Terminal",
    icon: <WindowConsole20Regular />,
    external: "terminal",
  },
  {
    section: "desktop",
    label: "Desktop",
    icon: <Desktop20Regular />,
    external: "desktop",
  },
];

const VALID_SECTIONS = ["overview", "chat"] as const;

const useStyles = makeStyles({
  layout: {
    display: "flex",
    height: "100%",
    overflow: "hidden",
    "@media (max-width: 640px)": { flexDirection: "column" },
  },
  sidebar: {
    width: "180px",
    boxSizing: "border-box",
    "@media (max-width: 640px)": { width: "100%" },
  },
  navigation: {
    "@media (max-width: 640px)": {
      display: "flex",
      flexWrap: "wrap",
      gap: "4px",
    },
  },
  navButton: {
    width: "100%",
    "@media (max-width: 640px)": { width: "auto", flexGrow: 1 },
  },
});

export function WorkspacePage() {
  const styles = useStyles();
  const { instanceName, section } = useParams<{
    instanceName: string;
    section?: string;
  }>();
  const navigate = useNavigate();
  const {
    data: instance,
    isLoading: instanceLoading,
    isError: instanceError,
    error,
  } = useInstanceByNameQuery(instanceName);
  const viewer = useMe();
  const features = useFeatures();
  const chatAccount = useChatAccount();
  const chatSessionExpired = chatAccount?.error === SESSION_EXPIRED_MESSAGE;
  const isError = instanceError || !!viewer.error || chatSessionExpired;
  const isLoading = !isError && (instanceLoading || viewer.isLoading);
  const errorMessage = chatSessionExpired ? SESSION_EXPIRED_MESSAGE
    : viewer.error instanceof Error ? viewer.error.message
    : error?.message ?? "Failed to load agent. Please refresh the page.";
  const chatEligible =
    instance?.agent.type === "custom" &&
    instance.agent.port != null &&
    !instance.agent.webUI;
  const chatEnabled = features.data?.customChatEnabled === true;

  // Default section is overview; redirect unknown sections
  if (
    !section ||
    !VALID_SECTIONS.includes(section as (typeof VALID_SECTIONS)[number])
  ) {
    return <Navigate to={`/instances/${instanceName}/overview`} replace />;
  }

  const externalUrls = {
    agent: instance && !isError ? agentUIUrl(instance) : undefined,
    terminal: instance && !isError ? terminalUrl(instance) : undefined,
    desktop: instance && !isError ? desktopUrl(instance) : undefined,
  };
  const hasWebUI = instance?.agent.type === "custom" && !!instance.agent.webUI;

  if (section === "chat" && instance && !isError) {
    if (!chatEligible)
      return <Navigate to={`/instances/${instanceName}/overview`} replace />;
    if (features.isLoading)
      return <div style={{ padding: 32 }}>Checking Chat availability…</div>;
    if (!chatEnabled)
      return <Navigate to={`/instances/${instanceName}/overview`} replace />;
    return (
      <Suspense fallback={<div style={{ padding: 32 }}>Opening chat…</div>}>
        <ChatPage
          key={`${instance.uid}:${chatAccount?.scope ?? ""}`}
          instance={instance}
        />
      </Suspense>
    );
  }

  return (
    <div className={styles.layout}>
      {/* Sidebar */}
      <div
        className={styles.sidebar}
        style={{
          flexShrink: 0,
          display: "flex",
          flexDirection: "column",
          backgroundColor: tokens.colorNeutralBackground2,
          borderRight: `1px solid ${tokens.colorNeutralStroke1}`,
        }}
      >
        {/* Back link */}
        <div
          style={{
            padding: "12px 8px",
            borderBottom: `1px solid ${tokens.colorNeutralStroke1}`,
          }}
        >
          <Button
            appearance="subtle"
            icon={<ArrowLeft20Regular />}
            onClick={() => navigate("/")}
            style={{ width: "100%", justifyContent: "flex-start" }}
          >
            All Agents
          </Button>
        </div>

        {/* Instance name + phase */}
        <div
          style={{
            padding: "12px 16px",
            borderBottom: `1px solid ${tokens.colorNeutralStroke1}`,
          }}
        >
          <Text
            weight="semibold"
            size={300}
            style={{ overflowWrap: "anywhere" }}
          >
            {instance?.displayName.trim() || instanceName}
          </Text>
          {instance && (
            <div style={{ marginTop: 4 }}>
              <Badge
                appearance="filled"
                color={phaseBadgeColor[instance.phase] ?? "informative"}
                size="small"
              >
                {instance.phase}
              </Badge>
            </div>
          )}
        </div>

        {/* Nav items */}
        <div
          className={styles.navigation}
          role="navigation"
          aria-label="Agent workspace"
          style={{ flex: 1, padding: "8px 0" }}
        >
          {NAV_ITEMS.map((item) => {
            if (item.section === "chat" && (!chatEligible || !chatEnabled))
              return null;
            const externalUrl = item.external
              ? externalUrls[item.external]
              : undefined;
            const externalDisabled =
              Boolean(item.external) &&
              (instance?.phase !== "Ready" || !externalUrl);

            return (
              <Button
                key={item.section}
                className={styles.navButton}
                as={item.external ? "a" : undefined}
                href={item.external ? externalUrl : undefined}
                target={
                  item.external === "agent" && hasWebUI ? "_blank" : undefined
                }
                rel={
                  item.external === "agent" && hasWebUI
                    ? "noopener noreferrer"
                    : undefined
                }
                disabled={externalDisabled}
                appearance={section === item.section ? "subtle" : "transparent"}
                icon={item.icon}
                onClick={
                  item.external
                    ? undefined
                    : () =>
                        navigate(`/instances/${instanceName}/${item.section}`)
                }
                style={{
                  justifyContent: "flex-start",
                  borderRadius: 0,
                  fontWeight: section === item.section ? 600 : 400,
                  borderLeft:
                    section === item.section
                      ? `3px solid ${tokens.colorBrandForeground1}`
                      : "3px solid transparent",
                }}
              >
                {item.external === "agent" && hasWebUI ? "Web UI" : item.label}
              </Button>
            );
          })}
        </div>
      </div>

      {/* Content area */}
      <div
        style={{
          flex: 1,
          minWidth: 0,
          minHeight: 0,
          overflow: "hidden",
          display: "flex",
          flexDirection: "column",
        }}
      >
        {isLoading && (
          <div style={{ padding: 24 }}>
            <Text>Loading...</Text>
          </div>
        )}

        {!isLoading && isError && (
          <div role="alert" style={{ padding: 24 }}>
            <Text style={{ color: tokens.colorStatusDangerForeground1 }}>
              {errorMessage}
            </Text>
          </div>
        )}

        {!isLoading && !isError && !instance && (
          <div style={{ padding: 24 }}>
            <Text style={{ color: tokens.colorStatusDangerForeground1 }}>
              Agent not found.
            </Text>
          </div>
        )}

        {instance && !isError && section === "overview" && (
          <OverviewSection key={instance.uid} instance={instance} />
        )}
      </div>
    </div>
  );
}
