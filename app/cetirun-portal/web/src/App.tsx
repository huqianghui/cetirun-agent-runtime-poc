// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Routes, Route, Navigate } from "react-router-dom";
import { Header } from "./components/Header";
import { NoticeBanner } from "./components/NoticeBanner";
import { AdminGuard } from "./components/AdminGuard";
import { InstanceListPage } from "./pages/InstanceListPage";
import { WorkspacePage } from "./pages/WorkspacePage";
import { ConnectedServicesPage } from "./pages/ConnectedServicesPage";
import { AdminLayout } from "./pages/admin/AdminLayout";
import { TeamListPage } from "./pages/admin/TeamListPage";
import { TeamEditPage } from "./pages/admin/TeamEditPage";
import { PolicyListPage } from "./pages/admin/PolicyListPage";
import { SandboxListPage } from "./pages/admin/SandboxListPage";
import { SandboxEditPage } from "./pages/admin/SandboxEditPage";
import { PolicyEditPage } from "./pages/admin/PolicyEditPage";
import { SkillMarketplacePage } from "./pages/admin/SkillMarketplacePage";
import { InstanceListPage as AdminInstanceListPage } from "./pages/admin/InstanceListPage";
import { InstanceDetailPage as AdminInstanceDetailPage } from "./pages/admin/InstanceDetailPage";
import { AIGatewayPage } from "./pages/admin/AIGatewayPage";
import "./components/Layout.css";

export default function App() {
  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        height: "100dvh",
        overflow: "hidden",
      }}
    >
      <Header />
      <NoticeBanner />
      <div
        style={{
          flex: 1,
          overflow: "hidden",
          display: "flex",
          flexDirection: "column",
        }}
      >
        <Routes>
          <Route path="/" element={<InstanceListPage />} />
          <Route path="/connections" element={<ConnectedServicesPage />} />
          <Route
            path="/instances/:instanceName/:section"
            element={<WorkspacePage />}
          />
          <Route path="/instances/:instanceName" element={<WorkspacePage />} />
          <Route path="/admin" element={<AdminGuard />}>
            <Route element={<AdminLayout />}>
              <Route index element={<Navigate to="teams" replace />} />
              <Route path="teams" element={<TeamListPage />} />
              <Route path="teams/new" element={<TeamEditPage />} />
              <Route path="teams/:name/edit" element={<TeamEditPage />} />
              <Route path="policies" element={<PolicyListPage />} />
              <Route path="policies/new" element={<PolicyEditPage />} />
              <Route path="policies/:name/edit" element={<PolicyEditPage />} />
              <Route path="sandboxes" element={<SandboxListPage />} />
              <Route path="sandboxes/new" element={<SandboxEditPage />} />
              <Route path="sandboxes/:name/edit" element={<SandboxEditPage />} />
              <Route path="skills" element={<SkillMarketplacePage />} />
              <Route path="instances" element={<AdminInstanceListPage />} />
              <Route path="instances/:name" element={<AdminInstanceDetailPage />} />
              <Route path="ai-gateway" element={<AIGatewayPage />} />
            </Route>
          </Route>
        </Routes>
      </div>
    </div>
  );
}
