// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import {
  Button,
  Select,
  Text,
  tokens,
  Checkbox,
  Dialog,
  DialogActions,
  DialogBody,
  DialogContent,
  DialogSurface,
  DialogTitle,
} from "@fluentui/react-components";
import { lazy, Suspense, useState } from "react";
import { Link, NavLink } from "react-router-dom";
import { useThemeContext, type ThemeMode } from "../context/ThemeContext";
import { useFeatures } from "../hooks/useFeatures";
import { useMe } from "../context/MeContext";
import cetirunLogo from "../assets/cetirun.png";
import { useChatAccount } from "../chat/ChatAccount";

const HistoryDrawer = lazy(() => import("../chat/HistoryDrawer"));

const THEME_MODES: ThemeMode[] = ["system", "light", "dark"];

function isThemeMode(value: string): value is ThemeMode {
  return (THEME_MODES as string[]).includes(value);
}

export function buildSignOutUrl(location: {
  protocol: string;
  host: string;
  pathname: string;
  search: string;
  hash: string;
}): string {
  const { protocol, host, pathname, search, hash } = location;
  const returnTo = `${protocol}//${host}${pathname}${search}${hash}`;
  return `/oauth2/sign_out?rd=${encodeURIComponent(returnTo)}`;
}

export function Header() {
  const { themeMode, setThemeMode } = useThemeContext();
  const { me } = useMe();
  const { data: features } = useFeatures();
  const account = useChatAccount();
  const [historyOpen, setHistoryOpen] = useState(false);
  const [signOutOpen, setSignOutOpen] = useState(false);
  const [clearHistory, setClearHistory] = useState(false);
  const [signOutError, setSignOutError] = useState<string>();
  const finishSignOut = async (clear: boolean) => {
    try {
      await account?.signOut(clear);
      window.location.href = buildSignOutUrl(window.location);
    } catch {
      setSignOutError(
        "Local records could not be cleared. You can stay here or sign out without clearing them.",
      );
    }
  };
  const handleSignOut = () => {
    if (account?.scope) {
      setSignOutOpen(true);
      return;
    }
    window.location.href = buildSignOutUrl(window.location);
  };

  return (
    <header
      style={{
        borderBottom: `1px solid ${tokens.colorNeutralStroke1}`,
      }}
    >
      <div
        className="portal-header-inner"
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
        }}
      >
        <Link
          to="/"
          aria-label="CetiRun Portal home"
          className="portal-header-brand"
          style={{
            display: "flex",
            alignItems: "center",
            gap: "8px",
            color: "inherit",
            textDecoration: "none",
          }}
        >
          <img src={cetirunLogo} alt="CetiRun" width={48} height={48} />
          <Text size={500} weight="semibold">
            CetiRun Portal
          </Text>
        </Link>
        <div className="portal-header-actions">
          {(features?.agentToolsEnabled || me?.isAdmin) && (
            <nav className="portal-header-navigation" aria-label="Main navigation">
              {me?.isAdmin && (
                <NavLink
                  to="/admin"
                  style={({ isActive }) => ({
                    textDecoration: "none",
                    padding: "4px 8px",
                    borderRadius: "4px",
                    fontSize: "14px",
                    color: isActive
                      ? tokens.colorBrandForeground1
                      : tokens.colorNeutralForeground1,
                    fontWeight: isActive ? 600 : 400,
                  })}
                >
                  Admin
                </NavLink>
              )}
              {features?.agentToolsEnabled && (
                <NavLink
                  to="/connections"
                  style={({ isActive }) => ({
                    textDecoration: "none",
                    padding: "4px 8px",
                    borderRadius: "4px",
                    fontSize: "14px",
                    color: isActive
                      ? tokens.colorBrandForeground1
                      : tokens.colorNeutralForeground1,
                    fontWeight: isActive ? 600 : 400,
                  })}
                >
                  Connections
                </NavLink>
              )}
            </nav>
          )}
          <div className="portal-header-utilities">
            {account?.scope && !account.checking && (
              <Button
                appearance="subtle"
                size="small"
                onClick={() => setHistoryOpen(true)}
              >
                Local history
              </Button>
            )}
            <label className="portal-header-theme">
              <Text size={200}>Theme</Text>
              <Select
                size="small"
                aria-label="Theme"
                value={themeMode}
                onChange={(_, data) => {
                  if (isThemeMode(data.value)) setThemeMode(data.value);
                }}
              >
                <option value="system">System</option>
                <option value="light">Light</option>
                <option value="dark">Dark</option>
              </Select>
            </label>
            <Button appearance="subtle" size="small" onClick={handleSignOut}>
              Sign out
            </Button>
          </div>
        </div>
      </div>
      {account?.scope && !account.checking && historyOpen && (
        <Suspense fallback={null}>
          <HistoryDrawer
            key={account.scope}
            scope={account.scope}
            open={historyOpen}
            onClose={() => setHistoryOpen(false)}
          />
        </Suspense>
      )}
      <Dialog
        open={signOutOpen}
        onOpenChange={(_, data) => setSignOutOpen(data.open)}
      >
        <DialogSurface>
          <DialogBody>
            <DialogTitle>Sign out of CetiRun?</DialogTitle>
            <DialogContent>
              <p>
                Signing out keeps chat history on this browser. On a shared
                computer, clear your local history before leaving.
              </p>
              <Checkbox
                checked={clearHistory}
                onChange={(_, data) => setClearHistory(data.checked === true)}
                label="Clear this account's local chat history"
              />
              {signOutError && <p role="alert">{signOutError}</p>}
            </DialogContent>
            <DialogActions>
              <Button onClick={() => setSignOutOpen(false)}>Stay here</Button>
              {signOutError && (
                <Button onClick={() => void finishSignOut(true)}>
                  Retry clearing
                </Button>
              )}
              <Button
                appearance="primary"
                onClick={() =>
                  void finishSignOut(signOutError ? false : clearHistory)
                }
              >
                {signOutError ? "Sign out without clearing" : "Sign out"}
              </Button>
            </DialogActions>
          </DialogBody>
        </DialogSurface>
      </Dialog>
    </header>
  );
}
