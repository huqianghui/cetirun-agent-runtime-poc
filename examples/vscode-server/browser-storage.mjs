// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// A path prefix is not a browser security boundary. This adapter keeps the
// editor's cooperative storage and callback keys separate on a shared origin.
export function scopeBrowserStorage(namespace) {
  const scope = globalThis;
  if (scope.__cetirunStorageNamespace) return;
  Object.defineProperty(scope, "__cetirunStorageNamespace", { value: namespace });
  const prefix = `cetirun-vscode:${namespace}:`;
  const stores = new Map();
  for (const property of ["localStorage", "sessionStorage"]) {
    if (!(property in scope)) continue;
    const native = scope[property];
    const keys = () => Array.from({ length: native.length }, (_, i) => native.key(i))
      .filter((key) => key?.startsWith(prefix)).map((key) => key.slice(prefix.length));
    const methods = {
      getItem: (key) => native.getItem(prefix + String(key)),
      setItem: (key, value) => native.setItem(prefix + String(key), String(value)),
      removeItem: (key) => native.removeItem(prefix + String(key)),
      clear: () => keys().forEach((key) => native.removeItem(prefix + key)),
      key: (index) => keys()[index] ?? null,
    };
    const view = new Proxy(Object.create(null), {
      get: (_, key) => key === "length" ? keys().length :
        Object.hasOwn(methods, key) ? methods[key] : typeof key === "string" ? methods.getItem(key) ?? undefined : undefined,
      set: (_, key, value) => { methods.setItem(key, value); return true; },
      deleteProperty: (_, key) => { methods.removeItem(key); return true; },
      ownKeys: keys,
      has: (_, key) => key === "length" || Object.hasOwn(methods, key) || methods.getItem(key) !== null,
      getOwnPropertyDescriptor: (_, key) => methods.getItem(key) === null ? undefined :
        { configurable: true, enumerable: true, writable: true, value: methods.getItem(key) },
    });
    stores.set(native, view);
    Object.defineProperty(scope, property, { configurable: true, get: () => view });
  }
  if (scope.indexedDB) {
    const db = scope.indexedDB;
    const open = db.open.bind(db);
    const remove = db.deleteDatabase.bind(db);
    db.open = (name, ...args) => open(prefix + String(name), ...args);
    db.deleteDatabase = (name, ...args) => remove(prefix + String(name), ...args);
    if (db.databases) {
      const list = db.databases.bind(db);
      db.databases = async () => (await list()).filter((entry) => entry.name?.startsWith(prefix))
        .map((entry) => ({ ...entry, name: entry.name.slice(prefix.length) }));
    }
  }
  if (scope.BroadcastChannel) {
    const NativeChannel = scope.BroadcastChannel;
    scope.BroadcastChannel = class extends NativeChannel {
      constructor(name) { super(prefix + String(name)); }
    };
  }
  if (scope.caches) {
    const cache = scope.caches;
    for (const method of ["open", "has", "delete"]) {
      const original = cache[method].bind(cache);
      cache[method] = (name) => original(prefix + String(name));
    }
    const list = cache.keys.bind(cache);
    cache.keys = async () => (await list()).filter((name) => name.startsWith(prefix)).map((name) => name.slice(prefix.length));
    cache.match = async (request, options = {}) => {
      for (const name of options.cacheName ? [options.cacheName] : await cache.keys()) {
        if (!await cache.has(name)) continue;
        const result = await (await cache.open(name)).match(request, options);
        if (result) return result;
      }
    };
  }
  const callbacks = new WeakMap();
  const add = scope.addEventListener?.bind(scope);
  const remove = scope.removeEventListener?.bind(scope);
  if (add && remove && scope.StorageEvent) {
    scope.addEventListener = (type, listener, options) => {
      if (type !== "storage" || !listener) return add(type, listener, options);
      if (!callbacks.has(listener)) callbacks.set(listener, (event) => {
        if (!event.key?.startsWith(prefix)) return;
        const scoped = new StorageEvent("storage", {
          key: event.key.slice(prefix.length), oldValue: event.oldValue, newValue: event.newValue, url: event.url,
        });
        Object.defineProperty(scoped, "storageArea", { value: stores.get(event.storageArea) });
        if (typeof listener === "function") listener.call(scope, scoped);
        else listener.handleEvent(scoped);
      });
      return add(type, callbacks.get(listener), options);
    };
    scope.removeEventListener = (type, listener, options) => remove(type,
      type === "storage" ? callbacks.get(listener) ?? listener : listener, options);
  }
}
