// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { NavLink, Outlet } from "react-router-dom";
import { Text, tokens } from "@fluentui/react-components";
import { useFeatures } from "../../hooks/useFeatures";

const navStyle = ({ isActive }: { isActive: boolean }) => ({
  display: "block",
  padding: "8px 12px",
  borderRadius: "4px",
  textDecoration: "none",
  fontSize: "14px",
  color: isActive ? tokens.colorBrandForeground1 : tokens.colorNeutralForeground1,
  background: isActive ? tokens.colorNeutralBackground2 : "transparent",
  fontWeight: isActive ? 600 : 400,
});

export function AdminLayout() {
  const { data: features } = useFeatures();
  return (
    <div style={{ display: "flex", flex: 1, overflow: "hidden" }}>
      <aside
        style={{
          width: "200px",
          borderRight: `1px solid ${tokens.colorNeutralStroke1}`,
          padding: "16px 8px",
          display: "flex",
          flexDirection: "column",
          gap: "4px",
        }}
      >
        <Text size={300} weight="semibold" style={{ padding: "0 12px 8px" }}>
          Admin
        </Text>
        <NavLink to="/admin/teams" style={navStyle}>
          Teams
        </NavLink>
        <NavLink to="/admin/policies" style={navStyle}>
          Agent policies
        </NavLink>
        <NavLink to="/admin/sandboxes" style={navStyle}>
          Sandboxes
        </NavLink>
        {features?.skillMarketplaceEnabled && (
          <NavLink to="/admin/skills" style={navStyle}>
            Skills
          </NavLink>
        )}
        <NavLink to="/admin/instances" style={navStyle}>
          Agents
        </NavLink>
        <NavLink to="/admin/ai-gateway" style={navStyle}>
          AI Gateway
        </NavLink>
        <span
          style={{
            ...navStyle({ isActive: false }),
            color: tokens.colorNeutralForegroundDisabled,
            cursor: "not-allowed",
          }}
        >
          Settings
        </span>
      </aside>
      <main style={{ flex: 1, overflow: "auto", padding: "24px" }}>
        <Outlet />
      </main>
    </div>
  );
}
