// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

export function scopeWebappStorage(): void {
  const prefix = (window as Window & { __HERMES_BASE_PATH__?: string }).__HERMES_BASE_PATH__
  if (typeof prefix !== 'string' || !/^\/ui\/[a-z0-9][a-z0-9-]*$/.test(prefix)) return

  const marker = Symbol.for('cetirun.webapp.storage-scope.v1')
  if (Reflect.has(window, marker)) return
  const namespace = `cetirun.webapp:${encodeURIComponent(prefix)}:`
  const stores = new Map<Storage, Storage>()

  for (const kind of ['localStorage', 'sessionStorage'] as const) {
    let original: Storage
    try {
      original = window[kind]
    } catch {
      continue
    }
    if (kind === 'sessionStorage') {
      const launchKey = `hermes.webapp.session.v1:${JSON.stringify([window.location.origin, prefix])}`
      const launchGrant = original.getItem(launchKey)
      if (launchGrant !== null) {
        original.setItem(namespace + launchKey, launchGrant)
        original.removeItem(launchKey)
      }
    }
    const keys = (): string[] => {
      const selected: string[] = []
      for (let index = 0; index < original.length; index++) {
        const key = original.key(index)
        if (key?.startsWith(namespace)) selected.push(key.slice(namespace.length))
      }
      return selected
    }
    const methods = {
      getItem: (key: string) => original.getItem(namespace + String(key)),
      setItem: (key: string, value: string) => original.setItem(namespace + String(key), String(value)),
      removeItem: (key: string) => original.removeItem(namespace + String(key)),
      clear: () => keys().forEach(key => original.removeItem(namespace + key)),
      key: (index: number) => keys()[Number(index) >>> 0] ?? null
    }
    const scoped = new Proxy(Object.create(Storage.prototype) as Storage, {
      get(target, key, receiver) {
        if (key === 'length') return keys().length
        if (Object.hasOwn(methods, key)) return methods[key as keyof typeof methods]
        if (typeof key === 'string') return methods.getItem(key) ?? Reflect.get(target, key, receiver)
        return Reflect.get(target, key, receiver)
      },
      set(_target, key, value) {
        if (typeof key !== 'string') return false
        methods.setItem(key, value)
        return true
      },
      deleteProperty(_target, key) {
        if (typeof key !== 'string') return false
        methods.removeItem(key)
        return true
      },
      has(target, key) {
        return (typeof key === 'string' && methods.getItem(key) !== null) || Reflect.has(target, key)
      },
      ownKeys: keys,
      getOwnPropertyDescriptor(_target, key) {
        if (typeof key !== 'string') return undefined
        const value = methods.getItem(key)
        return value === null ? undefined : { configurable: true, enumerable: true, writable: true, value }
      },
      defineProperty(_target, key, descriptor) {
        if (typeof key !== 'string' || !Object.hasOwn(descriptor, 'value')) return false
        methods.setItem(key, descriptor.value)
        return true
      }
    })
    stores.set(original, scoped)
    Object.defineProperty(window, kind, { configurable: true, get: () => scoped })
  }

  const translated = new WeakSet<StorageEvent>()
  window.addEventListener('storage', event => {
    if (translated.has(event) || (!event.isTrusted && event.storageArea === null)) return
    event.stopImmediatePropagation()
    const storage = event.storageArea && stores.get(event.storageArea)
    if (!storage || (event.key !== null && !event.key.startsWith(namespace))) return
    const scoped = new StorageEvent('storage', {
      key: event.key === null ? null : event.key.slice(namespace.length),
      oldValue: event.oldValue, newValue: event.newValue, url: event.url
    })
    Object.defineProperty(scoped, 'storageArea', { value: storage })
    translated.add(scoped)
    window.dispatchEvent(scoped)
  }, true)

  if (typeof BroadcastChannel !== 'undefined') {
    const OriginalChannel = BroadcastChannel
    window.BroadcastChannel = class extends OriginalChannel {
      constructor(name: string) { super(namespace + String(name)) }
      get name(): string { return super.name.slice(namespace.length) }
    }
  }
  Object.defineProperty(window, marker, { value: prefix })
}
