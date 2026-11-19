// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useMe } from "../context/MeContext";
import { isSessionExpiredError } from "../api/fetch";
import {
  getInstance,
  createInstance,
  deleteInstanceByName,
  updateInstance,
  updateInstanceByName,
  upgradeInstance,
  upgradeInstanceByName,
  restartInstanceByName,
  setInstanceSuspendedByName,
  getInstanceStatus,
  getInstanceStatusByName,
  fetchInstances,
  fetchInstanceByName,
} from "../api/instance";
import type { Instance, InstanceVersion, CreateInstanceRequest, UpdateInstanceRequest, RestartInstanceRequest, SetInstanceSuspendedRequest } from "../types/instance";

const INSTANCE_KEY = ["instance"] as const;

function useViewerKey() {
  const { me, error } = useMe();
  return me && (!error || isSessionExpiredError(error)) ? `${me.alias}:${me.email.toLowerCase()}` : undefined;
}

export function useInstanceQuery() {
  const viewer = useViewerKey();
  return useQuery({
    queryKey: [...INSTANCE_KEY, viewer],
    queryFn: () => getInstance(),
    enabled: Boolean(viewer),
    refetchInterval: (query) => {
      // Stop polling when the session has expired to avoid wasting resources.
      if (query.state.error?.message.includes("Session expired")) return false;

      const data = query.state.data;
      if (
        data &&
        (data.phase === "Provisioning" ||
          data.phase === "Pending" ||
          data.phase === "Deleting")
      ) {
        return 5_000;
      }
      return 30_000;
    },
    retry: (failureCount, error) => {
      // Don't retry on session expiry - it will just keep failing.
      if (error.message.includes("Session expired")) return false;
      return failureCount < 1;
    },
  });
}

export function useCreateInstance(options?: { onSuccess?: () => void; onSettled?: () => void }) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: async (req: CreateInstanceRequest) => {
      try {
        return await createInstance(req);
      } finally {
        // React Query retains mutation variables after settlement.
        delete req.registryAuth;
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: INSTANCE_KEY });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
      options?.onSuccess?.();
    },
    onSettled: options?.onSettled,
  });
}

export function useDeleteInstanceByName(name: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (version: InstanceVersion) => deleteInstanceByName(name, version),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: INSTANCE_KEY });
      queryClient.invalidateQueries({ queryKey: ["instance", name] });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
    },
  });
}

export function useUpdateInstance() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (req: UpdateInstanceRequest) => updateInstance(req),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: INSTANCE_KEY });
    },
  });
}

export function useUpdateInstanceByName(name: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (req: UpdateInstanceRequest) => updateInstanceByName(name, req),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["instance", name] });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
    },
  });
}

export function useUpgradeInstance() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (version: InstanceVersion) => upgradeInstance(version),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: INSTANCE_KEY });
    },
  });
}

export function useUpgradeInstanceByName(name: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (version: InstanceVersion) => upgradeInstanceByName(name, version),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["instance", name] });
      queryClient.invalidateQueries({ queryKey: ["instance", name, "status"] });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
    },
  });
}

export function useRestartInstanceByName(name: string) {
  const queryClient = useQueryClient();
  const viewer = useViewerKey();
  return useMutation({
    mutationFn: (request: RestartInstanceRequest) => restartInstanceByName(name, request),
    onSuccess: (instance) => {
      queryClient.setQueryData(["instance", name, viewer], instance);
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: ["instance", name] });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
    },
  });
}

export function useSetInstanceSuspendedByName(name: string) {
  const queryClient = useQueryClient();
  const viewer = useViewerKey();
  return useMutation({
    mutationFn: (request: SetInstanceSuspendedRequest) => setInstanceSuspendedByName(name, request),
    retry: false,
    onSuccess: (instance, request) => {
      queryClient.setQueryData<Instance>(["instance", name, viewer], (current) => {
        if (current && (current.uid !== request.uid || (current.generation ?? 0) > (instance.generation ?? 0))) return current;
        // Status-only writes do not advance generation, and resourceVersions
        // are opaque. Keep a refreshed observation until invalidation refetches.
        if (current && current.generation === instance.generation && current.resourceVersion !== request.resourceVersion) return current;
        return instance;
      });
    },
    onSettled: () => {
      queryClient.invalidateQueries({ queryKey: INSTANCE_KEY });
      queryClient.invalidateQueries({ queryKey: ["instances"] });
    },
  });
}

export function useInstanceStatusQuery(enabled: boolean) {
  const viewer = useViewerKey();
  return useQuery({
    queryKey: ["instance", "status", viewer],
    queryFn: () => getInstanceStatus(),
    enabled: enabled && Boolean(viewer),
    refetchInterval: (query) => {
      if (query.state.error?.message.includes("Session expired")) return false;
      return 10_000;
    },
    refetchIntervalInBackground: false,
    retry: (failureCount, error) => {
      if (error.message.includes("Session expired")) return false;
      return failureCount < 1;
    },
  });
}

export function useInstanceStatusByNameQuery(name: string, enabled: boolean, uid?: string) {
  const viewer = useViewerKey();
  return useQuery({
    queryKey: ["instance", name, "status", viewer, uid],
    queryFn: () => getInstanceStatusByName(name),
    enabled: enabled && Boolean(viewer) && Boolean(uid),
    refetchInterval: (query) => {
      if (query.state.error?.message.includes("Session expired")) return false;
      return 10_000;
    },
    refetchIntervalInBackground: false,
    retry: (failureCount, error) => {
      if (error.message.includes("Session expired")) return false;
      return failureCount < 1;
    },
  });
}

export function useInstancesQuery() {
  const viewer = useViewerKey();
  return useQuery({
    queryKey: ["instances", viewer],
    queryFn: fetchInstances,
    enabled: Boolean(viewer),
    refetchInterval: (query) => {
      if (query.state.error?.message.includes("Session expired")) return false;
      return query.state.data?.some((instance) => instance.phase === "Pending" || instance.phase === "Provisioning" ||
        instance.phase === "Deleting" || (instance.generation ?? 0) > (instance.observedGeneration ?? 0)) ? 5_000 : 30_000;
    },
    retry: (count, error) => !error.message.includes("Session expired") && count < 1,
  });
}

export function useInstanceByNameQuery(name: string | undefined) {
  const viewer = useViewerKey();
  return useQuery({
    queryKey: ["instance", name, viewer],
    queryFn: () => fetchInstanceByName(name!),
    enabled: !!name && Boolean(viewer),
    refetchInterval: (query) => {
      if (query.state.error?.message.includes("Session expired")) return false;
      const instance = query.state.data;
      return instance && (instance.phase === "Pending" || instance.phase === "Provisioning" || instance.phase === "Deleting" ||
        (instance.generation ?? 0) > (instance.observedGeneration ?? 0)) ? 5_000 : 30_000;
    },
    retry: (count, error) => !error.message.includes("Session expired") && count < 1,
  });
}
