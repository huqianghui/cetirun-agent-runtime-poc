// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { Outlet } from "react-router-dom";
import { Spinner } from "@fluentui/react-components";
import { useMe } from "../context/MeContext";
import { ForbiddenPage } from "../pages/ForbiddenPage";

export function AdminGuard() {
  const { me, isLoading } = useMe();
  if (isLoading) {
    return (
      <div style={{ display: "flex", alignItems: "center", justifyContent: "center", flex: 1 }}>
        <Spinner label="Loading..." />
      </div>
    );
  }
  // A background refetch failure can set `error` while `me` is still
  // populated from the previous successful fetch. Only treat the absence
  // of `me` as fatal so a transient blip doesn't kick a logged-in admin
  // out of the admin UI.
  if (!me) {
    return <ForbiddenPage message="Unable to verify your identity." />;
  }
  if (!me.isAdmin) {
    return <ForbiddenPage message="Admin access required." />;
  }
  return <Outlet />;
}
