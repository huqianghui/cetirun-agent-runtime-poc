// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import type { ReactNode } from "react";
import { Header } from "./Header";
import { NoticeBanner } from "./NoticeBanner";
import "./Layout.css";

export function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="layout-root">
      <Header />
      <div style={{ padding: "12px 48px 0" }}>
        <NoticeBanner />
      </div>
      <main style={{ margin: "24px 48px 0" }}>
        {children}
      </main>
    </div>
  );
}
