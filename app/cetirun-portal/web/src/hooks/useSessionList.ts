// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export const PAGE_SIZE = 10;

export type SessionSummary = {
  id: string;
  title: string;
  lastActivity: number;
};

export type SessionList = {
  sessions: SessionSummary[];
  hasMore: boolean;
  isLoading: boolean;
  loadMore: () => void;
  refresh: () => void;
};

export function sortByActivityDesc(sessions: SessionSummary[]): SessionSummary[] {
  return [...sessions].sort((a, b) => b.lastActivity - a.lastActivity);
}

export function relativeTime(then: number, now: number = Date.now()): string {
  const diff = Math.max(0, now - then);
  const min = Math.floor(diff / 60_000);
  if (min < 1) return "just now";
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  const day = Math.floor(hr / 24);
  return `${day}d ago`;
}
